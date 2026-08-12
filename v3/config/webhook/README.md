# Webhook CA injection

The base `ValidatingWebhookConfiguration` intentionally contains a non-empty
sentinel in `clientConfig.caBundle`. It is not a trust anchor. Before applying
this directory to a cluster, a deployment overlay must replace
`webhooks[0].clientConfig.caBundle` with the base64-encoded PEM CA that signs
the `agw-operator-webhook` serving certificate.

The supported injection contract is:

```yaml
metadata:
  annotations:
    agents.astatide.com/ca-bundle-source: kustomize-replacement:agw-webhook-ca.data.caBundle
```

For example, a deployment-owned overlay can provide the CA through an env file
and replace the sentinel without cert-manager:

```yaml
configMapGenerator:
  - name: agw-webhook-ca
    envs:
      - webhook-ca.env       # caBundle=<base64-encoded PEM CA>
replacements:
  - source:
      kind: ConfigMap
      name: agw-webhook-ca
      fieldPath: data.caBundle
    targets:
      - select:
          kind: ValidatingWebhookConfiguration
          name: agw-operator-validating-webhook
        fieldPaths:
          - webhooks.0.clientConfig.caBundle
```

The source value must be base64-encoded PEM, not raw PEM. Keep the overlay and
its CA source under deployment control; no private key or credential belongs in
this repository.

The webhook uses `failurePolicy: Fail`. Until the overlay injects a real CA,
the API server will reject AgentRun admission instead of silently allowing an
unverified run. Certificate issuance and rotation are intentionally left to
the deployment's PKI mechanism; cert-manager is not required by this base.
