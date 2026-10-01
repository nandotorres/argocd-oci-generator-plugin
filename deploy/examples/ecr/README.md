# Authenticating to Amazon ECR

The plugin fetches ECR credentials from the **AWS default credential chain** and
optionally **assumes a role** (STS) before calling `ecr:GetAuthorizationToken`.
It never reads static AWS keys from the ApplicationSet — credentials come from
the pod's cloud identity and the server-side config only (DESIGN.md §5).

Authorization tokens are cached per `region|roleArn` and refreshed ~15 min
before their 12h expiry, so the hot path does no AWS calls.

## Pick an identity mechanism (no static keys)

| Scenario | How | `roleArn` in plugin config? |
|---|---|---|
| **IRSA** (IAM Roles for Service Accounts) | Annotate the ServiceAccount with `eks.amazonaws.com/role-arn`. Pod gets a web-identity token automatically. | Omit for same-account; set for cross-account. |
| **EKS Pod Identity** (newer, no OIDC setup) | `aws eks create-pod-identity-association` binding the SA to a role. | Omit for same-account; set for cross-account. |
| **Cross-account ECR** | Base identity (IRSA/Pod Identity) **assumes** a role in the ECR account. | **Set** `roleArn` to the ECR-account role. |

Both IRSA and Pod Identity are picked up automatically by the AWS SDK — you do
not configure keys anywhere.

## Files here

| File | Purpose |
|---|---|
| `serviceaccount-irsa.yaml` | ServiceAccount wired for IRSA (used by the plugin Deployment). |
| `configmap-server-ecr.yaml` | Plugin server config with an ECR registry (same- and cross-account shown). |
| `applicationset-ecr.yaml` | ApplicationSet that discovers and deploys from ECR. |
| `iam-policy-ecr-read.json` | Least-privilege **read-only** ECR policy for the role. |
| `iam-trust-irsa.json` | Trust policy for the IRSA role (OIDC web-identity). |
| `iam-trust-crossaccount.json` | Trust policy for the ECR-account role assumed cross-account. |

## Good practices baked into these examples

1. **No static credentials.** Use IRSA or Pod Identity; nothing to leak or rotate.
2. **Least privilege.** The role only gets read actions; `ecr:GetAuthorizationToken`
   must be `Resource: "*"` (it's account-scoped), but every repo action is
   constrained to `repository/apps-oci/*` ARNs.
3. **Defense in depth.** The plugin's `allowedRepositories` allowlist independently
   restricts which repositories an ApplicationSet may target, even if IAM is broad.
4. **Immutability downstream.** Prefer `{{ .oci.pinnedRef }}` / `{{ .oci.digest }}`
   in templates so deployments are pinned to a content digest, not a mutable tag.
5. **Keep traffic in-VPC.** On private clusters, add ECR **VPC endpoints**
   (`com.amazonaws.<region>.ecr.api`, `com.amazonaws.<region>.ecr.dkr`) and the
   S3 gateway endpoint so pulls don't traverse the internet.
6. **Enable ECR tag immutability + scan-on-push** on the repositories themselves.

## Wiring it up

```sh
# 1. Create the IAM role with the read policy + the right trust policy, e.g. IRSA:
aws iam create-role --role-name oci-generator-irsa \
  --assume-role-policy-document file://iam-trust-irsa.json
aws iam put-role-policy --role-name oci-generator-irsa \
  --policy-name ecr-read --policy-document file://iam-policy-ecr-read.json

# 2. Point the ServiceAccount at the role (edit the ARN first):
kubectl apply -f serviceaccount-irsa.yaml

# 3. Configure the plugin server + an ApplicationSet:
kubectl apply -f configmap-server-ecr.yaml
kubectl apply -f applicationset-ecr.yaml
```

### Cross-account specifics

- Set `auth.roleArn` in `configmap-server-ecr.yaml` to the role in the **ECR
  account**.
- That role uses `iam-trust-crossaccount.json` (trusts the base IRSA/Pod-Identity
  role) and attaches `iam-policy-ecr-read.json`.
- The base role additionally needs `sts:AssumeRole` on the target role.
- The **ECR repository policy** in the ECR account must allow the target role to
  pull (`ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer`).
