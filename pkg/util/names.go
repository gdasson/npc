package util

import (
    "crypto/sha1"
    "encoding/hex"
    "strings"
)

func SafeTaskName(runName, nodeName string) string {
    base := "task-" + runName + "-" + nodeName
    base = strings.ToLower(base)
    base = strings.ReplaceAll(base, ".", "-")
    base = strings.ReplaceAll(base, "/", "-")
    base = strings.ReplaceAll(base, "_", "-")
    if len(base) <= 253 {
        return base
    }
    h := sha1.Sum([]byte(runName + "|" + nodeName))
    return "task-" + runName + "-" + hex.EncodeToString(h[:8])
}
