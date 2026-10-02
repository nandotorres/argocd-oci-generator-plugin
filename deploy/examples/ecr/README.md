# ECR extras

Same-account IRSA install is in the [root README](../../../README.md#ecr).
This folder is the IAM snippets and the two less-common variants.

| file | |
|---|---|
| `iam-trust-irsa.json` | IRSA trust. Replace account, region, OIDC id. |
| `iam-trust-crossaccount.json` | Trust for a role in the ECR account. |
| `iam-policy-ecr-read.json` | Read-only ECR. Replace account, region, repo prefix. |
| `serviceaccount-irsa.yaml` | SA annotation (`eks.amazonaws.com/role-arn`). |
| `configmap-server-ecr.yaml` | Plugin config (`auth.type: ecr`). |
| `applicationset-ecr.yaml` | Example ApplicationSet. |

`ecr:GetAuthorizationToken` has to be `Resource: "*"`. Everything else in the
policy is scoped to `repository/apps-oci/*`. The plugin `allowedRepositories`
list is a second filter on top of IAM.

Tokens are cached per `region|roleArn` and refreshed about 15 minutes before
the 12h expiry.

## Pod Identity

No SA annotation. Bind the same role:

```bash
aws eks create-pod-identity-association \
  --cluster-name YOUR_CLUSTER \
  --namespace argocd \
  --service-account oci-generator \
  --role-arn arn:aws:iam::111122223333:role/oci-generator-irsa
```

The AWS SDK picks this up the same way as IRSA. Leave `roleArn` out of the
plugin config for same-account.

## Cross-account

Pod runs in account A, images live in account B.

1. Role in B (`ecr-read-for-oci-generator`) attaches `iam-policy-ecr-read.json`
   and trusts the pod role via `iam-trust-crossaccount.json`.
2. Pod role in A needs `sts:AssumeRole` on that B role.
3. Plugin config for B's registry sets `auth.roleArn` to the B role.
4. B's ECR repository policy must allow the B role to pull
   (`ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer`, …).

## Private clusters

Add VPC endpoints for `ecr.api`, `ecr.dkr`, and the S3 gateway, or the plugin
cannot reach ECR.
