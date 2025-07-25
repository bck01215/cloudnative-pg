/*
Copyright © contributors to CloudNativePG, established as
CloudNativePG a Series of LF Projects, LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package controller

import (
	"context"
	"crypto/x509"
	"fmt"

	"github.com/cloudnative-pg/machinery/pkg/log"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/certs"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
)

// setupPostgresPKI create all the PKI infrastructure that PostgreSQL need to work
// if using ssl=on
func (r *ClusterReconciler) setupPostgresPKI(ctx context.Context, cluster *apiv1.Cluster) error {
	// This is the CA of cluster
	serverCaSecret, err := r.ensureServerCASecret(ctx, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("missing specified server CA secret %s: %w", cluster.GetServerCASecretName(), err)
		}
		return fmt.Errorf("generating server CA certificate: %w", err)
	}

	if err = r.ensureServerLeafCertificate(ctx, cluster, serverCaSecret); err != nil {
		return fmt.Errorf("generating server TLS certificate: %w", err)
	}

	clientCaSecret, err := r.ensureClientCASecret(ctx, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("missing specified client CA secret %s: %w", cluster.GetClientCASecretName(), err)
		}
		return fmt.Errorf("generating client CA certificate: %w", err)
	}

	err = r.ensureReplicationClientLeafCertificate(ctx, cluster, clientCaSecret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("missing specified streaming replication client TLS secret %s: %w",
				cluster.Status.Certificates.ReplicationTLSSecret, err)
		}
		return fmt.Errorf("generating streaming replication client certificate: %w", err)
	}

	return nil
}

// ensureClientCASecret ensure that the cluster CA really exist and is valid
func (r *ClusterReconciler) ensureClientCASecret(ctx context.Context, cluster *apiv1.Cluster) (*v1.Secret, error) {
	if cluster.Spec.Certificates == nil || cluster.Spec.Certificates.ClientCASecret == "" {
		return r.ensureCASecret(ctx, cluster, cluster.GetClientCASecretName())
	}

	var secret v1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.GetNamespace(), Name: cluster.GetClientCASecretName()},
		&secret)
	// If specified and error, bubble up
	if err != nil {
		r.Recorder.Event(cluster, "Warning", "SecretNotFound",
			"Getting secret "+cluster.GetClientCASecretName())
		return nil, err
	}

	err = r.verifyCAValidity(ctx, secret, cluster)
	if err != nil {
		return nil, err
	}

	// Validate also ca.key if needed
	if cluster.Spec.Certificates.ReplicationTLSSecret == "" {
		_, err = certs.ParseCASecret(&secret)
		if err != nil {
			r.Recorder.Event(cluster, "Warning", "InvalidCASecret",
				fmt.Sprintf("Parsing client secret %s: %s", secret.Name, err.Error()))
			return nil, err
		}
	}

	// If specified and found, go on
	return &secret, nil
}

// ensureServerCASecret ensure that the cluster CA really exist and is valid
func (r *ClusterReconciler) ensureServerCASecret(ctx context.Context, cluster *apiv1.Cluster) (*v1.Secret, error) {
	// If not specified, use default amd renew/generate
	certificates := cluster.Spec.Certificates
	if certificates == nil || certificates.ServerCASecret == "" {
		return r.ensureCASecret(ctx, cluster, cluster.GetServerCASecretName())
	}

	var secret v1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.GetNamespace(), Name: cluster.GetServerCASecretName()},
		&secret)
	// If specified and error, bubble up
	if err != nil {
		r.Recorder.Event(cluster, "Warning", "SecretNotFound",
			"Getting secret "+cluster.GetServerCASecretName())
		return nil, err
	}

	err = r.verifyCAValidity(ctx, secret, cluster)
	if err != nil {
		return nil, err
	}

	// validate also ca.key if needed
	if cluster.Spec.Certificates.ServerTLSSecret == "" {
		_, err = certs.ParseCASecret(&secret)
		if err != nil {
			r.Recorder.Event(cluster, "Warning", "InvalidCASecret",
				fmt.Sprintf("Parsing server secret %s: %s", secret.Name, err.Error()))
			return nil, err
		}
	}

	// If specified and found, go on
	return &secret, nil
}

func (r *ClusterReconciler) verifyCAValidity(ctx context.Context, secret v1.Secret, cluster *apiv1.Cluster) error {
	contextLogger := log.FromContext(ctx)

	// Verify validity of the CA and expiration (only ca.crt)
	publicKey, ok := secret.Data[certs.CACertKey]
	if !ok {
		return fmt.Errorf("missing %s secret data", certs.CACertKey)
	}

	caPair := &certs.KeyPair{
		Certificate: publicKey,
	}

	isExpiring, _, err := caPair.IsExpiring()
	if err != nil {
		return err
	} else if isExpiring {
		r.Recorder.Event(cluster, "Warning", "SecretIsExpiring",
			"Checking expiring date of secret "+secret.Name)
		contextLogger.Info("CA certificate is expiring or is already expired", "secret", secret.Name)
	}

	return nil
}

func (r *ClusterReconciler) ensureCASecret(ctx context.Context, cluster *apiv1.Cluster,
	secretName string,
) (*v1.Secret, error) {
	var secret v1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.GetNamespace(), Name: secretName}, &secret)
	if err == nil {
		// Verify the validity of this CA and renew it if needed
		err = r.renewCASecret(ctx, &secret)
		if err != nil {
			return nil, err
		}

		return &secret, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	caPair, err := certs.CreateRootCA(cluster.Name, cluster.Namespace)
	if err != nil {
		return nil, fmt.Errorf("while creating the CA of the cluster: %w", err)
	}

	derivedCaSecret := caPair.GenerateCASecret(cluster.Namespace, secretName)
	utils.SetAsOwnedBy(&derivedCaSecret.ObjectMeta, cluster.ObjectMeta, cluster.TypeMeta)
	err = r.Create(ctx, derivedCaSecret)

	return derivedCaSecret, err
}

// renewCASecret check if this CA secret is valid and renew it if needed
func (r *ClusterReconciler) renewCASecret(ctx context.Context, secret *v1.Secret) error {
	pair, err := certs.ParseCASecret(secret)
	if err != nil {
		return err
	}

	expiring, _, err := pair.IsExpiring()
	if err != nil {
		return err
	}
	if !expiring {
		return nil
	}

	privateKey, err := pair.ParseECPrivateKey()
	if err != nil {
		return err
	}

	err = pair.RenewCertificate(privateKey, nil, nil)
	if err != nil {
		return err
	}

	secret.Data[certs.CACertKey] = pair.Certificate
	return r.Update(ctx, secret)
}

// ensureServerLeafCertificate checks if we have a certificate for PostgreSQL and generate/renew it
func (r *ClusterReconciler) ensureServerLeafCertificate(
	ctx context.Context,
	cluster *apiv1.Cluster,
	caSecret *v1.Secret,
) error {
	// This is the certificate for the server
	secretName := client.ObjectKey{Namespace: cluster.GetNamespace(), Name: cluster.GetServerTLSSecretName()}

	// If not specified generate/renew
	if cluster.Spec.Certificates == nil || cluster.Spec.Certificates.ServerTLSSecret == "" {
		return r.ensureLeafCertificate(
			ctx,
			cluster,
			secretName,
			cluster.GetServiceReadWriteName(),
			caSecret,
			certs.CertTypeServer,
			cluster.GetClusterAltDNSNames(),
			nil,
		)
	}

	var serverSecret v1.Secret
	if err := r.Get(ctx, secretName, &serverSecret); apierrors.IsNotFound(err) {
		return fmt.Errorf("missing specified server TLS secret %s: %w",
			cluster.Status.Certificates.ServerTLSSecret, err)
	} else if err != nil {
		return err
	}

	opts := &x509.VerifyOptions{KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	return validateLeafCertificate(caSecret, &serverSecret, opts)
}

// ensureServerLeafCertificate checks if we have a client certificate for the
// streaming_replica user and generate/renew it
func (r *ClusterReconciler) ensureReplicationClientLeafCertificate(
	ctx context.Context,
	cluster *apiv1.Cluster,
	caSecret *v1.Secret,
) error {
	// Generating postgres client certificate
	replicationSecretObjectKey := client.ObjectKey{
		Namespace: cluster.GetNamespace(),
		Name:      cluster.GetReplicationSecretName(),
	}

	// If not specified generate/renew
	if cluster.Spec.Certificates == nil || cluster.Spec.Certificates.ReplicationTLSSecret == "" {
		return r.ensureLeafCertificate(
			ctx,
			cluster,
			replicationSecretObjectKey,
			apiv1.StreamingReplicationUser,
			caSecret,
			certs.CertTypeClient,
			nil,
			nil,
		)
	}

	var replicationClientSecret v1.Secret
	if err := r.Get(ctx, replicationSecretObjectKey, &replicationClientSecret); apierrors.IsNotFound(err) {
		return fmt.Errorf("missing specified replication TLS secret %s: %w",
			replicationSecretObjectKey.Name, err)
	} else if err != nil {
		return err
	}

	opts := &x509.VerifyOptions{KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	return validateLeafCertificate(caSecret, &replicationClientSecret, opts)
}

func validateLeafCertificate(caSecret *v1.Secret, serverSecret *v1.Secret, opts *x509.VerifyOptions) error {
	publicKey, ok := caSecret.Data[certs.CACertKey]
	if !ok {
		return fmt.Errorf("missing %s secret data", certs.CACertKey)
	}

	caPair := &certs.KeyPair{Certificate: publicKey}

	serverPair, err := certs.ParseServerSecret(serverSecret)
	if err != nil {
		return err
	}

	return serverPair.IsValid(caPair, opts)
}

// ensureLeafCertificate check if we have a certificate for PostgreSQL and generate/renew it
func (r *ClusterReconciler) ensureLeafCertificate(
	ctx context.Context,
	cluster *apiv1.Cluster,
	secretName client.ObjectKey,
	commonName string,
	caSecret *v1.Secret,
	usage certs.CertType,
	altDNSNames []string,
	additionalLabels map[string]string,
) error {
	var secret v1.Secret
	err := r.Get(ctx, secretName, &secret)
	switch {
	case err == nil:
		return r.renewAndUpdateCertificate(ctx, caSecret, &secret, altDNSNames)
	case apierrors.IsNotFound(err):
		serverSecret, err := generateCertificateFromCA(caSecret, commonName, usage, altDNSNames, secretName)
		if err != nil {
			return err
		}

		utils.SetAsOwnedBy(&serverSecret.ObjectMeta, cluster.ObjectMeta, cluster.TypeMeta)
		for k, v := range additionalLabels {
			if serverSecret.Labels == nil {
				serverSecret.Labels = make(map[string]string)
			}
			serverSecret.Labels[k] = v
		}
		return r.Create(ctx, serverSecret)
	default:
		return err
	}
}

// generateCertificateFromCA create a certificate secret using the provided CA secret
func generateCertificateFromCA(
	caSecret *v1.Secret,
	commonName string,
	usage certs.CertType,
	altDNSNames []string,
	secretName client.ObjectKey,
) (*v1.Secret, error) {
	caPair, err := certs.ParseCASecret(caSecret)
	if err != nil {
		return nil, err
	}

	serverPair, err := caPair.CreateAndSignPair(commonName, usage, altDNSNames)
	if err != nil {
		return nil, err
	}

	serverSecret := serverPair.GenerateCertificateSecret(secretName.Namespace, secretName.Name)
	return serverSecret, nil
}

// renewAndUpdateCertificate renew a certificate giving the certificate that contains the CA that sign it and update
// the secret
func (r *ClusterReconciler) renewAndUpdateCertificate(
	ctx context.Context,
	caSecret *v1.Secret,
	secret *v1.Secret,
	altDNSNames []string,
) error {
	origSecret := secret.DeepCopy()
	hasBeenRenewed, err := certs.RenewLeafCertificate(caSecret, secret, altDNSNames)
	if err != nil {
		return err
	}
	if hasBeenRenewed {
		return r.Patch(ctx, secret, client.MergeFrom(origSecret))
	}

	return nil
}
