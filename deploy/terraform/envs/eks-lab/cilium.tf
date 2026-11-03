# ---------------------------------------------------------------------------
# Cilium in CNI chaining mode on top of the AWS VPC CNI.
#
# The VPC CNI keeps doing IPAM/routing (so pod IPs stay VPC-routable and the
# ALB ip targets keep working); Cilium attaches eBPF programs on top for
# NetworkPolicy enforcement and Hubble observability. This is what Rule 8
# (hubble_drop_*) and Rule 3's CT-map metrics (cilium_*) consume — same
# metric setup as the on-prem lab (docs/deploy-onprem.md).
#
# Chaining values per the official guide (installation/cni-chaining-aws-cni):
# chainingMode=aws-cni, exclusive=false, no masquerade, native routing.
# ---------------------------------------------------------------------------

resource "helm_release" "cilium" {
  name       = "cilium"
  repository = "https://helm.cilium.io"
  chart      = "cilium"
  version    = "1.18.2"
  namespace  = "kube-system"

  values = [yamlencode({
    cni = {
      chainingMode = "aws-cni"
      exclusive    = false # don't rename the aws-cni config out from under it
    }
    enableIPv4Masquerade = false    # ENI IPs route natively in the VPC
    routingMode          = "native" # no tunnel needed on top of ENI routing

    # Same metric surface the NetworkDoctor chart's PodMonitor scrapes on-prem.
    prometheus = {
      enabled = true # cilium_* agent metrics on :9962
    }
    hubble = {
      metrics = {
        enabled = ["dns", "drop", "tcp", "flow"] # hubble_* on :9965; Rule 8 reads "drop"
      }
    }
  })]

  depends_on = [module.eks]
}
