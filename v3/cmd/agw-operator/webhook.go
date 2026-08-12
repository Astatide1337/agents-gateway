package main

import (
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func registerWebhooks(mgr ctrl.Manager, handler admission.Handler) {
	mgr.GetWebhookServer().Register(defaultWebhookPath, &webhook.Admission{
		Handler: handler,
	})
}
