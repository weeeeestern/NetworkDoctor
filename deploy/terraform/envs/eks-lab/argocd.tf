# ---------------------------------------------------------------------------
# Argo CD — the last platform component Terraform installs. Everything above
# it (kube-prometheus-stack, networkdoctor, demo, holmesgpt) is deployed as
# Argo Applications from deploy/bootstrap/eks-lab.yaml, registered by hand:
#
#   kubectl apply -f deploy/argocd/root-application-eks-lab.yaml
#
# UI: kubectl -n argocd port-forward svc/argocd-server 8443:443
# Password: kubectl -n argocd get secret argocd-initial-admin-secret \
#             -o jsonpath='{.data.password}' | base64 -d
# ---------------------------------------------------------------------------

resource "helm_release" "argocd" {
  name             = "argocd"
  repository       = "https://argoproj.github.io/argo-helm"
  chart            = "argo-cd"
  version          = "10.11.7"
  namespace        = "argocd"
  create_namespace = true
  timeout          = 600

  depends_on = [
    module.eks,
    helm_release.cilium, # pods come up already Cilium-managed
  ]
}
