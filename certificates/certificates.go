package certificates

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/cert"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	certName       = "tls.crt"
	keyName        = "tls.key"
	APIRuleCRDName = "apirules.gateway.kyma-project.io"

	secretNamespace = "cert-gen"
	secretName      = "api-gateway-webhook-service"
	serviceName     = "api-gateway-webhook-service"
)

func SetupCertificates() string {
	serverClient, err := ctrlclient.New(ctrl.GetConfigOrDie(), ctrlclient.Options{})
	if err != nil {
		return fmt.Sprintf("failed to create a server client: %s", err.Error())
	}

	if err := apiextensionsv1.AddToScheme(serverClient.Scheme()); err != nil {
		return fmt.Sprintf("while adding apiextensions.v1 schema to k8s client: %s", err.Error())
	}

	if err := ensureWebhookCertificate(context.TODO(), serverClient, secretName, secretNamespace, serviceName); err != nil {
		return fmt.Sprintf("failed to ensure webhook secret: %s", err.Error())
	}

	return "success"
}

func createCABundle(webhookNamespace string, serviceName string) ([]byte, []byte, error) {
	cert, key, err := createCert(webhookNamespace, serviceName)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to crete cert")
	}
	return cert, key, nil
}

func addCertToConversionWebhook(ctx context.Context, client ctrlclient.Client, caBundle []byte) error {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := client.Get(ctx, types.NamespacedName{Name: APIRuleCRDName}, crd)
	if err != nil {
		return errors.Wrap(err, "failed to get APIRule crd")
	}

	if contains, msg := containsConversionWebhookClientConfig(crd); !contains {
		return errors.Errorf("while validating CRD to be CaBundle injectable,: %s", msg)
	}

	crd.Spec.Conversion.Webhook.ClientConfig.CABundle = caBundle
	err = client.Update(ctx, crd)
	if err != nil {
		return errors.Wrap(err, "while updating CRD with Conversion webhook caBundle")
	}
	return nil
}

func ensureWebhookCertificate(ctx context.Context, client ctrlclient.Client, secretName, secretNamespace, serviceName string) error {
	secret := &corev1.Secret{}

	err := client.Get(ctx, types.NamespacedName{Name: secretName, Namespace: secretNamespace}, secret)
	if err != nil && !apiErrors.IsNotFound(err) {
		return errors.Wrap(err, "failed to get webhook secret")
	}

	if apiErrors.IsNotFound(err) {
		return createSecret(ctx, client, secretName, secretNamespace, serviceName)
	}

	if err := updateSecret(ctx, client, secret, serviceName); err != nil {
		return errors.Wrap(err, "failed to update secret")
	}
	return nil
}

func createSecret(ctx context.Context, client ctrlclient.Client, name, namespace, serviceName string) error {
	cert, key, err := buildCert(namespace, serviceName)
	if err != nil {
		return errors.Wrap(err, "failed to build cert ")
	}

	secret := buildSecret(name, namespace, cert, key)

	if err := client.Create(ctx, secret); err != nil {
		return errors.Wrap(err, "failed to create secret")
	}

	err = addCertToConversionWebhook(ctx, client, cert)
	if err != nil {
		return err
	}
	return nil
}

func containsConversionWebhookClientConfig(crd *apiextensionsv1.CustomResourceDefinition) (bool, string) {
	if crd.Spec.Conversion == nil {
		return false, "conversion not found in APIRule CRD"
	}

	if crd.Spec.Conversion.Webhook == nil {
		return false, "conversion webhook not found in APIRule CRD"
	}

	if crd.Spec.Conversion.Webhook.ClientConfig == nil {
		return false, "client config for conversion webhook not found in APIRule CRD"
	}
	return true, ""
}

func createCert(webhookNamespace string, serviceName string) ([]byte, []byte, error) {
	cert, key, err := buildCert(webhookNamespace, serviceName)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to build certificate")
	}

	return cert, key, nil
}

func isValidSecret(s *corev1.Secret) (bool, error) {
	if !hasRequiredKeys(s.Data) {
		return false, nil
	}
	if err := verifyCertificate(s.Data[certName]); err != nil {
		return false, err
	}
	if err := verifyKey(s.Data[keyName]); err != nil {
		return false, err
	}

	return true, nil
}

func verifyCertificate(c []byte) error {
	certificate, err := cert.ParseCertsPEM(c)
	if err != nil {
		return errors.Wrap(err, "failed to parse certificate data")
	}
	// certificate is self signed. So we use it as a root cert
	root, err := cert.NewPoolFromBytes(c)
	if err != nil {
		return errors.Wrap(err, "failed to parse root certificate data")
	}
	// make sure the certificate is valid for the next 10 days. Otherwise it will be recreated.
	_, err = certificate[0].Verify(x509.VerifyOptions{CurrentTime: time.Now().Add(10 * 24 * time.Hour), Roots: root})
	if err != nil {
		return errors.Wrap(err, "certificate verification failed")
	}
	return nil
}

func verifyKey(k []byte) error {
	b, _ := pem.Decode(k)
	key, err := x509.ParsePKCS1PrivateKey(b.Bytes)
	if err != nil {
		return errors.Wrap(err, "failed to parse key data")
	}
	if err = key.Validate(); err != nil {
		return errors.Wrap(err, "key verification failed")
	}
	return nil
}

func hasRequiredKeys(data map[string][]byte) bool {
	if data == nil {
		return false
	}
	for _, key := range []string{certName, keyName} {
		if _, ok := data[key]; !ok {
			return false
		}
	}
	return true
}

func buildCert(namespace, serviceName string) (cert []byte, key []byte, err error) {
	cert, key, err = generateWebhookCertificates(serviceName, namespace)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate webhook certificates")
	}

	return cert, key, nil
}

func updateSecret(ctx context.Context, client ctrlclient.Client, secret *corev1.Secret, serviceName string) error {
	valid, _ := isValidSecret(secret)
	if valid {
		return nil
	}

	cert, key, err := createCABundle(secret.Namespace, serviceName)
	if err != nil {
		return errors.Wrap(err, "failed to ensure webhook secret")
	}

	newSecret := buildSecret(secret.Name, secret.Namespace, cert, key)

	secret.Data = newSecret.Data
	if err := client.Update(ctx, secret); err != nil {
		return errors.Wrap(err, "failed to update secret")
	}

	if err := addCertToConversionWebhook(ctx, client, cert); err != nil {
		return errors.Wrap(err, "while adding CaBundle to Conversion Webhook for function CRD")
	}
	return nil
}

func generateWebhookCertificates(serviceName, namespace string) ([]byte, []byte, error) {
	altNames := serviceAltNames(serviceName, namespace)
	return cert.GenerateSelfSignedCertKey(altNames[0], nil, altNames)
}

func serviceAltNames(serviceName, namespace string) []string {
	namespacedServiceName := strings.Join([]string{serviceName, namespace}, ".")
	commonName := strings.Join([]string{namespacedServiceName, "svc"}, ".")
	serviceHostname := fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, namespace)

	return []string{
		commonName,
		serviceName,
		namespacedServiceName,
		serviceHostname,
	}
}

func buildSecret(name, namespace string, cert []byte, key []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: v1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			certName: cert,
			keyName:  key,
		},
	}
}
