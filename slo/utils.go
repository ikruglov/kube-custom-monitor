package slo

import (
	"fmt"
	"strings"

	"k8s.io/api/core/v1"
)

func fetchContainerName(objRef v1.ObjectReference) (string, error) {
	name := objRef.FieldPath
	if strings.HasPrefix(name, "spec.containers{") {
		name = strings.TrimPrefix(name, "spec.containers{")
		name = strings.TrimSuffix(name, "}")
		return name, nil
	}

	if strings.HasPrefix(name, "spec.initContainers{") {
		name = strings.TrimPrefix(name, "spec.initContainers{")
		name = strings.TrimSuffix(name, "}")
		return name, nil
	}

	return "", fmt.Errorf("unknown format: %s", name)
}

func getPodKey(namespace, name, uid string) string {
	return namespace + "/" + name + "/" + uid
}

func getPodKeyFromReference(objRef v1.ObjectReference) string {
	return getPodKey(objRef.Namespace, objRef.Name, string(objRef.UID))
}
