/*
 * Copyright (C) 2022  Appvia Ltd <info@appvia.io>
 *
 * This program is free software; you can redistribute it and/or
 * modify it under the terms of the GNU General Public License
 * as published by the Free Software Foundation; either version 2
 * of the License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package server

import (
	"bytes"
	"context"
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/appvia/terranetes-controller/pkg/register"
	"github.com/appvia/terranetes-controller/pkg/schema"
	"github.com/appvia/terranetes-controller/pkg/utils"
	"github.com/appvia/terranetes-controller/pkg/utils/kubernetes"
	"github.com/appvia/terranetes-controller/pkg/version"
)

// manageWebhooks is responsible for registering or unregistering the webhooks
func (s *Server) manageWebhooks(ctx context.Context, managed bool) error {
	cc, err := client.New(s.cfg, client.Options{Scheme: schema.GetScheme()})
	if err != nil {
		return err
	}
	log.WithField("managed", managed).Info("attempting to manage the controller webhooks")

	// @step: read the certificate authority
	ca, err := os.ReadFile(s.config.TLSAuthority)
	if err != nil {
		return fmt.Errorf("failed to read the certificate authority file, %w", err)
	}

	documents, err := utils.YAMLDocuments(bytes.NewReader(register.MustAsset("webhooks/manifests.yaml")))
	if err != nil {
		return fmt.Errorf("failed to decode the webhooks manifests, %w", err)
	}

	var webhookNamePrefix string
	if s.config.EnableWebhookPrefix {
		webhookNamePrefix = version.Name + "-"
	}

	// @step: register the validating webhooks
	for _, x := range documents {
		o, err := schema.DecodeYAML([]byte(x))
		if err != nil {
			return fmt.Errorf("failed to decode the webhook, %w", err)
		}
		o.SetName(webhookNamePrefix + o.GetName())

		switch o := o.(type) {
		case *admissionv1.ValidatingWebhookConfiguration:
			for i := 0; i < len(o.Webhooks); i++ {
				o.Webhooks[i].ClientConfig.CABundle = ca
				o.Webhooks[i].ClientConfig.Service.Namespace = os.Getenv("KUBE_NAMESPACE")
				o.Webhooks[i].ClientConfig.Service.Name = "controller"
				o.Webhooks[i].ClientConfig.Service.Port = ptr.To(int32(443))
			}

		case *admissionv1.MutatingWebhookConfiguration:
			for i := 0; i < len(o.Webhooks); i++ {
				o.Webhooks[i].ClientConfig.CABundle = ca
				o.Webhooks[i].ClientConfig.Service.Namespace = os.Getenv("KUBE_NAMESPACE")
				o.Webhooks[i].ClientConfig.Service.Name = "controller"
				o.Webhooks[i].ClientConfig.Service.Port = ptr.To(int32(443))
			}

		default:
			return fmt.Errorf("expected a validating or mutating webhook, got %T", o)
		}

		switch managed {
		case true:
			if err := kubernetes.CreateOrForceUpdate(ctx, cc, o); err != nil {
				return fmt.Errorf("failed to create / update the webhook, %w", err)
			}

		default:
			log.WithFields(log.Fields{
				"webhook": o.GetName(),
			}).Info("deleting any previous webhooks")

			if err := kubernetes.DeleteIfExists(ctx, cc, o); err != nil {
				return fmt.Errorf("failed to delete any previous webhook, %w", err)
			}
		}
	}

	// @step: create a webhook for intercepting the namespaces
	decision := admissionv1.Fail
	sideEffects := admissionv1.SideEffectClassNone

	wh := &admissionv1.ValidatingWebhookConfiguration{}
	wh.Name = webhookNamePrefix + "validating-webhook-namespace"
	wh.Webhooks = []admissionv1.ValidatingWebhook{
		{
			AdmissionReviewVersions: []string{"v1"},
			ClientConfig: admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{
					Name:      "controller",
					Namespace: os.Getenv("KUBE_NAMESPACE"),
					Path:      ptr.To("/validate/terraform.appvia.io/namespaces"),
					Port:      ptr.To(int32(443)),
				},
				CABundle: ca,
			},
			FailurePolicy: &decision,
			Name:          "namespaces.terraform.appvia.io",
			Rules: []admissionv1.RuleWithOperations{
				{
					Operations: []admissionv1.OperationType{"DELETE"},
					Rule: admissionv1.Rule{
						APIGroups:   []string{""},
						APIVersions: []string{"v1"},
						Resources:   []string{"namespaces"},
					},
				},
			},
			SideEffects: &sideEffects,
		},
	}

	// @step: if we are not managing the webhooks we can delete them completely
	if !managed {
		log.WithFields(log.Fields{
			"webhook": wh.GetName(),
		}).Info("deleting any previous namespace webhooks")

		if err := kubernetes.DeleteIfExists(ctx, cc, wh); err != nil {
			return fmt.Errorf("failed to delete the namespace webhook, %w", err)
		}

		return nil
	}

	// @step: we manage the webhooks, we either need to create, update or delete
	// the namespace webhook based on the controller configuration
	switch s.config.EnableNamespaceProtection {
	case true:
		if err := kubernetes.CreateOrForceUpdate(ctx, cc, wh); err != nil {
			return fmt.Errorf("failed to create / update the namespace webhook, %w", err)
		}
	default:
		if err := kubernetes.DeleteIfExists(ctx, cc, wh); err != nil {
			return fmt.Errorf("failed to delete the namespace webhook, %w", err)
		}
	}

	return nil
}
