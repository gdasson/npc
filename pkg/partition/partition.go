package partition

import (
    "crypto/sha256"
    "encoding/hex"
    "sort"

    corev1 "k8s.io/api/core/v1"
)

const ZoneLabel = "topology.kubernetes.io/zone"

func MixedAZHalves(nodes []corev1.Node) (p0 []string, p1 []string, planVersion string) {
    byZone := map[string][]string{}
    for _, n := range nodes {
        z := n.Labels[ZoneLabel]
        byZone[z] = append(byZone[z], n.Name)
    }
    zones := make([]string, 0, len(byZone))
    for z := range byZone { zones = append(zones, z) }
    sort.Strings(zones)

    h := sha256.New()
    for _, z := range zones {
        list := byZone[z]
        sort.Strings(list)
        h.Write([]byte(z))
        for _, name := range list { h.Write([]byte(name)) }
        k := (len(list)+1)/2
        p0 = append(p0, list[:k]...)
        p1 = append(p1, list[k:]...)
    }
    sum := h.Sum(nil)
    planVersion = hex.EncodeToString(sum[:8])
    return
}
