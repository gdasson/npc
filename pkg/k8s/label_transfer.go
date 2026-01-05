package k8s

import (
    "sort"
    "strings"

    v1alpha1 "github.com/myorg/nodepatcher/api/v1alpha1"
)

type LabelPatch struct {
    Set    map[string]string
    Remove []string
}

func ComputeLabelDelta(oldLabels, newLabels map[string]string, spec *v1alpha1.LabelTransferSpec) LabelPatch {
    out := LabelPatch{Set: map[string]string{}}
    if spec == nil { return out }

    exclude := map[string]struct{}{}
    for _, k := range spec.ExcludeKeys { exclude[k] = struct{}{} }

    allowed := func(k string) bool {
        if _, bad := exclude[k]; bad { return false }
        if isReservedLabel(k) { return false }
        for _, ek := range spec.Keys {
            if k == ek { return true }
        }
        for _, p := range spec.Prefixes {
            if strings.HasPrefix(k, p) { return true }
        }
        return false
    }

    for k, oldV := range oldLabels {
        if !allowed(k) { continue }
        if newV, ok := newLabels[k]; !ok || newV != oldV {
            out.Set[k] = oldV
        }
    }

    removeMissing := false
    if spec.RemoveMissing != nil { removeMissing = *spec.RemoveMissing }
    if removeMissing {
        for k := range newLabels {
            if !allowed(k) { continue }
            if _, ok := oldLabels[k]; !ok {
                out.Remove = append(out.Remove, k)
            }
        }
        sort.Strings(out.Remove)
    }
    return out
}

func isReservedLabel(k string) bool {
    switch k {
    case "kubernetes.io/hostname":
        return true
    }
    if strings.HasPrefix(k, "kubernetes.io/") ||
        strings.HasPrefix(k, "beta.kubernetes.io/") ||
        strings.HasPrefix(k, "topology.kubernetes.io/") ||
        strings.HasPrefix(k, "failure-domain.beta.kubernetes.io/") {
        return true
    }
    if strings.HasPrefix(k, "node.kubernetes.io/") {
        return true
    }
    return false
}
