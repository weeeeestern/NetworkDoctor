# Prometheus rule files

The canonical smoke-test rule file lives inside the Helm chart so one file
feeds both delivery mechanisms:

    deploy/helm/networkdoctor/rules/networkdoctor-smoke-rules.yaml

- Prometheus Operator present (`prometheusrules.monitoring.coreos.com`):
  set `prometheusRule.enabled=true` and the chart renders it as a
  `PrometheusRule`.
- No Operator: set `ruleConfigMap.enabled=true`; the chart renders the same
  file into a ConfigMap. Mount it into the Prometheus pod and add the path to
  `rule_files:`; or load it by hand:

      kubectl -n <prometheus-ns> create configmap networkdoctor-rules \
        --from-file=deploy/helm/networkdoctor/rules/networkdoctor-smoke-rules.yaml

Validate before committing:

    promtool check rules deploy/helm/networkdoctor/rules/networkdoctor-smoke-rules.yaml
