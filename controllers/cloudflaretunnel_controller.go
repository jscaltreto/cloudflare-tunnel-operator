/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"

	"github.com/beezlabs-org/cloudflare-tunnel-operator/controllers/constants"
	"github.com/cloudflare/cloudflare-go"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cfv1 "github.com/beezlabs-org/cloudflare-tunnel-operator/api/v1alpha1"
	"github.com/beezlabs-org/cloudflare-tunnel-operator/controllers/models"
)

// CloudflareTunnelReconciler reconciles a CloudflareTunnel object
type CloudflareTunnelReconciler struct {
	client.Client
	*TunnelExpanded
	Scheme *runtime.Scheme
	logger *logr.Logger
}

type TunnelExpanded struct {
	cfv1.CloudflareTunnelSpec
	*cloudflare.API
	AccountToken string // contains the token for the cloudflare account
	AccountTag   string // contains the user id/tag for the cloudflare account
	Name         string // name of the CRD as well as the tunnel
	Namespace    string // namespace of the CRD
	TunnelID     string // tunnel ID as generated by the remote
	TunnelSecret string // the secret that is generated by us to create and then connect to the tunnel
}

//+kubebuilder:rbac:groups=cloudflare-tunnel-operator.beezlabs.app,resources=cloudflaretunnels,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudflare-tunnel-operator.beezlabs.app,resources=cloudflaretunnels/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudflare-tunnel-operator.beezlabs.app,resources=cloudflaretunnels/finalizers,verbs=update

func (r *CloudflareTunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lfc := log.FromContext(ctx)
	r.logger = &lfc
	lfc.Info("Reconciling...")
	namespacedName := req.NamespacedName

	var cloudflareTunnel cfv1.CloudflareTunnel
	if err := r.Get(ctx, namespacedName, &cloudflareTunnel); err != nil {
		lfc.Error(err, "could not fetch CloudflareTunnel")
		return ctrl.Result{}, err
	}
	lfc.V(1).Info("Resource fetched")

	r.TunnelExpanded = &TunnelExpanded{
		CloudflareTunnelSpec: cloudflareTunnel.Spec,
		Name:                 cloudflareTunnel.Name,
		Namespace:            cloudflareTunnel.Namespace,
		TunnelID:             cloudflareTunnel.Status.TunnelID,
	}

	if err := r.fetchDecodeSecret(ctx); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.createTunnelRemote(ctx); err != nil {
		return ctrl.Result{}, err
	}

	// this concludes checking the remote tunnel config
	secretCreate, err := r.createSecret(ctx, cloudflareTunnel)
	if err != nil {
		return ctrl.Result{}, err
	}

	// now we have to check the deployment status and reconcile
	url, err := r.getTargetURL(ctx)
	if err != nil {
		lfc.Error(err, "could not generate URL")
		return ctrl.Result{}, err
	}

	configMapCreate, err := r.createConfigMap(ctx, cloudflareTunnel, url)
	if err != nil {
		return ctrl.Result{}, err
	}

	if _, err = r.createDeployment(ctx, cloudflareTunnel, secretCreate, configMapCreate); err != nil {
		return ctrl.Result{}, err
	}

	// finally we need to check if a CNAME exists for the given domain and create if not
	if err = r.createDNSCNAME(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CloudflareTunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cfv1.CloudflareTunnel{}).
		//Owns(&appsv1.Deployment{}).
		Complete(r)
}

func (r *CloudflareTunnelReconciler) fetchDecodeSecret(ctx context.Context) error {
	// check if a secret name is mentioned in the resource or not
	// TokenSecretName is the name of the secret resource that contains the account id and account token
	if len(r.TokenSecretName) == 0 {
		err := fmt.Errorf("CredentialSecretName key does not exist")
		r.logger.Error(err, "CredentialSecretName not found")
		return err
	}

	var secret corev1.Secret
	// try to get a secret with the given name
	if err := r.Get(ctx, types.NamespacedName{
		Name:      r.TokenSecretName,
		Namespace: r.Namespace,
	}, &secret); err != nil {
		if errors.IsNotFound(err) {
			// write a log only if the secret was not found and not for other errors
			r.logger.Error(err, "could not find secret with name "+r.TokenSecretName)
		}
		return err
	}
	r.logger.V(1).Info("Secret fetched")

	// secret found, decode the token
	encodedToken, okCred := secret.Data["token"]
	encodedAccountID, okAccount := secret.Data["accountID"]

	if !okCred {
		err := fmt.Errorf("invalid key")
		r.logger.Error(err, "key credentials not found")
		return err
	}

	if !okAccount {
		err := fmt.Errorf("invalid key")
		r.logger.Error(err, "key accountID not found")
		return err
	}
	r.logger.V(1).Info("Secret decoded")

	r.AccountTag = string(encodedAccountID)
	r.AccountToken = string(encodedToken)
	return nil // everything good
}

func (r *CloudflareTunnelReconciler) createTunnelRemote(ctx context.Context) error {
	cf, err := cloudflare.NewWithAPIToken(r.AccountToken) // create new instance of cloudflare sdk
	r.API = cf
	if err != nil {
		r.logger.Error(err, "could not create cloudflare instance")
		return err
	}
	r.logger.V(1).Info("Cloudflare instance successfully created")

	cf.AccountID = r.AccountTag

	falsePointer := false // needed as the function below only accepts a *bool

	// first, we are checking if tunnels with the given name exists in the remote or not
	// if they exist, we will be getting one or more of them, since cloudflare allows duplicate named tunnels
	// if 2 or more exists, we check if the current CRD status already has the TunnelID or not
	// if it has, we check if the returned tunnels has one with the same connector id and use it
	// else, we cannot accurately figure out which one of them to use and error out
	tunnelListParams := cloudflare.TunnelListParams{
		AccountID: cf.AccountID,
		Name:      r.Name,
		IsDeleted: &falsePointer,
	}
	// check if tunnelID already existed as part of the resource Status
	if r.TunnelID != "" {
		tunnelListParams.UUID = r.TunnelID
	}
	tunnels, err := cf.Tunnels(ctx, tunnelListParams)
	if err != nil {
		r.logger.Error(err, "could not fetch tunnel list")
		return err
	}
	r.logger.V(1).Info("Existing tunnels fetched")

	var tunnel cloudflare.Tunnel

	if len(tunnels) >= 2 {
		err := fmt.Errorf("multiple tunnels exist")
		r.logger.Error(err, "2 or more tunnels already exists with the given name. Unable to choose between one of them")
		return err
	} else if len(tunnels) == 1 {
		// a single tunnel found with the same name, so we use that
		r.logger.Info("Tunnel already exists. Reconciling...")
		tunnel = tunnels[0]
	} else {
		r.logger.Info("Tunnel doesn't exist. Creating...")
		tunnelSecret, err := generateTunnelSecret() // generate a random secret to be used as the tunnel secret
		if err != nil {
			r.logger.Error(err, "could not generate tunnel secret")
			return err
		}
		r.logger.V(1).Info("Cloudflare Tunnel secret generated")

		tunnelParams := cloudflare.TunnelCreateParams{
			AccountID: cf.AccountID, // account is available after the sdk authenticates with the given secret
			Name:      r.Name,       // name of the tunnel is the same as the name of the CRD
			Secret:    tunnelSecret, // use the randomly generated secret
		}

		tunnel, err = cf.CreateTunnel(ctx, tunnelParams)
		if err != nil {
			r.logger.Error(err, "could not create the tunnel")
			return err
		}
	}
	r.TunnelID = tunnel.ID // assign the tunnelID from the created tunnel

	tunnelToken, err := cf.TunnelToken(ctx, cloudflare.TunnelTokenParams{
		AccountID: cf.AccountID,
		ID:        tunnel.ID,
	})
	if err != nil {
		r.logger.Error(err, "could not fetch tunnel token")
		return err
	}
	tunnelTokenDecodedBytes, err := base64.StdEncoding.DecodeString(tunnelToken)
	if err != nil {
		r.logger.Error(err, "could not decode tunnel token")
		return err
	}
	r.TunnelSecret = string(tunnelTokenDecodedBytes)
	return nil
}

func (r *CloudflareTunnelReconciler) createDNSCNAME(ctx context.Context) error {
	zoneID, err := r.ZoneIDByName(r.Zone)
	if err != nil {
		r.logger.Error(err, "could not fetch zone id")
		return err
	}
	dnsRecords, err := r.DNSRecords(ctx, zoneID, cloudflare.DNSRecord{
		Type: "CNAME",
		Name: r.Domain,
	})
	if err != nil {
		r.logger.Error(err, "could not fetch dns list")
		return err
	}
	if len(dnsRecords) != 0 {
		r.logger.V(1).Info("DNS record exists")
	} else {
		r.logger.V(1).Info("DNS record doesn't exist, creating")
		_, err = r.CreateDNSRecord(ctx, zoneID, cloudflare.DNSRecord{
			Type:    "CNAME",
			Name:    r.Domain,
			Content: r.TunnelID + constants.CNAMESuffix,
			TTL:     0,
		})
		if err != nil {
			r.logger.Error(err, "could not fetch tunnel token")
			return err
		}
	}
	return nil
}

func (r *CloudflareTunnelReconciler) createSecret(ctx context.Context, cloudflareTunnel cfv1.CloudflareTunnel) (*corev1.Secret, error) {
	// now first we create the secret containing the creds to the tunnel
	// this is fully contained in the fetched tunnel secret including the tunnel id and account tag
	var secretFetch corev1.Secret
	secretCreate := models.Secret(models.SecretModel{
		Name:         r.Name,
		Namespace:    r.Namespace,
		TunnelSecret: r.TunnelSecret,
		TunnelID:     r.TunnelID,
	}).GetSecret()

	// the secret needs to have an owner reference back to the controller
	if err := ctrl.SetControllerReference(&cloudflareTunnel, secretCreate, r.Scheme); err != nil {
		r.logger.Error(err, "could not create controller reference in secret")
		return nil, err
	}
	r.logger.V(1).Info("Owner Reference for Secret created")

	// try to get an existing secret with the given name
	if err := r.Get(ctx, types.NamespacedName{Name: secretCreate.Name, Namespace: r.Namespace}, &secretFetch); err != nil {
		if errors.IsNotFound(err) {
			// error due to secret not being present, so, create one
			r.logger.Info("creating secret...")
			if err := r.Create(ctx, secretCreate); err != nil {
				r.logger.Error(err, "could not create secret in cluster")
				return nil, err
			}
		}
		return nil, err
	} else {
		// secret exists, so update it to ensure it is consistent
		if err := r.Update(ctx, secretCreate); err != nil {
			r.logger.Error(err, "could not update secret")
			return nil, err
		}
	}
	return secretCreate, nil
}

func (r *CloudflareTunnelReconciler) createConfigMap(ctx context.Context, cloudflareTunnel cfv1.CloudflareTunnel, url string) (*corev1.ConfigMap, error) {
	// now first we create the configMap containing the configuration to the tunnel
	var configMapFetch corev1.ConfigMap
	configMapCreate, err := models.ConfigMap(models.ConfigMapModel{
		Name:      r.Name,
		Namespace: r.Namespace,
		Service:   url,
		TunnelID:  r.TunnelID,
		Domain:    r.Domain,
	}).GetConfigMap()
	if err != nil {
		return nil, err
	}

	// the secret needs to have an owner reference back to the controller
	if err := ctrl.SetControllerReference(&cloudflareTunnel, configMapCreate, r.Scheme); err != nil {
		r.logger.Error(err, "could not create controller reference in configMap")
		return nil, err
	}
	r.logger.V(1).Info("Owner Reference for ConfigMap created")

	// try to get an existing secret with the given name
	if err := r.Get(ctx, types.NamespacedName{Name: configMapCreate.Name, Namespace: r.Namespace}, &configMapFetch); err != nil {
		if errors.IsNotFound(err) {
			// error due to secret not being present, so, create one
			r.logger.Info("creating ConfigMap...")
			if err := r.Create(ctx, configMapCreate); err != nil {
				r.logger.Error(err, "could not create ConfigMap in cluster")
				return nil, err
			}
		}
		return nil, err
	} else {
		// secret exists, so update it to ensure it is consistent
		if err := r.Update(ctx, configMapCreate); err != nil {
			r.logger.Error(err, "could not update ConfigMap")
			return nil, err
		}
	}
	return configMapCreate, nil
}

func (r *CloudflareTunnelReconciler) createDeployment(ctx context.Context, cloudflareTunnel cfv1.CloudflareTunnel, secret *corev1.Secret, configMap *corev1.ConfigMap) (*appsv1.Deployment, error) {
	// now first we create the configMap containing the configuration to the tunnel
	var deploymentFetch appsv1.Deployment
	deploymentCreate := models.Deployment(models.DeploymentModel{
		Name:      r.Name,
		Namespace: r.Namespace,
		Replicas:  r.Replicas,
		TunnelID:  r.TunnelID,
		Secret:    secret,
		ConfigMap: configMap,
	}).GetDeployment()

	// the secret needs to have an owner reference back to the controller
	if err := ctrl.SetControllerReference(&cloudflareTunnel, deploymentCreate, r.Scheme); err != nil {
		r.logger.Error(err, "could not create controller reference in deployment")
		return nil, err
	}
	r.logger.V(1).Info("Owner Reference for deployment created")

	// try to get an existing deployment with the given name
	if err := r.Get(ctx, types.NamespacedName{Name: deploymentCreate.Name, Namespace: r.Namespace}, &deploymentFetch); err != nil {
		if errors.IsNotFound(err) {
			// error due to secret not being present, so, create one
			r.logger.Info("creating deployment...")
			if err := r.Create(ctx, deploymentCreate); err != nil {
				r.logger.Error(err, "could not create deployment in cluster")
				return nil, err
			}
		}
		return nil, err
	} else {
		// deployment exists, so update it to ensure it is consistent
		if err := r.Update(ctx, deploymentCreate); err != nil {
			r.logger.Error(err, "could not update deployment")
			return nil, err
		}
	}
	return deploymentCreate, nil
}

func (r *CloudflareTunnelReconciler) getTargetURL(ctx context.Context) (string, error) {
	// first get the url for the targeted service
	var targetService corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Name: r.Service.Name, Namespace: r.Service.Namespace}, &targetService); err != nil {
		if errors.IsNotFound(err) {
			// error due to service not being present
			r.logger.Error(err, "target service not present")
		}
		return "", err
	} else {
		// service exists, check if port is open
		var port corev1.ServicePort
		for _, servicePort := range targetService.Spec.Ports {
			if servicePort.Port == r.Service.Port {
				r.logger.V(1).Info("Ports matched")
				port = servicePort
				break
			}
		}
		if &port == nil {
			r.logger.Error(err, "port doesn't exist in service")
			return "", err
		}
	}

	// if the service is a LoadBalancer then use the ingress IP as the host
	if targetService.Spec.Type == corev1.ServiceTypeLoadBalancer {
		return r.Service.Protocol + "://" + targetService.Status.LoadBalancer.Ingress[0].IP + ":" + strconv.Itoa(int(r.Service.Port)), nil
	}
	// else generate the URL of the form `service-name.namespace:port`
	// see https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/#a-aaaa-records
	return r.Service.Protocol + "://" + r.Service.Name + "." + r.Service.Namespace + ":" + strconv.Itoa(int(r.Service.Port)), nil
}

func generateTunnelSecret() (string, error) {
	randomBytes := make([]byte, 32)
	_, err := rand.Read(randomBytes)
	return base64.StdEncoding.EncodeToString(randomBytes), err
}
