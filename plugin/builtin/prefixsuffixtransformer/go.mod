module sigs.k8s.io/kustomize/plugin/builtin/prefixsuffixtransformer

go 1.13

require (
	sigs.k8s.io/kustomize/api v0.3.1
	sigs.k8s.io/yaml v1.2.0
)

replace sigs.k8s.io/kustomize/api v0.3.1 => ../../../api
