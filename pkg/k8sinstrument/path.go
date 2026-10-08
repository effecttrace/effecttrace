package k8sinstrument

import (
	"net/http"
	"strings"
)

// RequestInfo is what can be derived from a Kubernetes API request line.
type RequestInfo struct {
	Verb        string
	APIGroup    string
	APIVersion  string
	Namespace   string
	Resource    string
	Name        string
	Subresource string
}

// Mutating reports whether the verb changes cluster state.
func (r RequestInfo) Mutating() bool {
	switch r.Verb {
	case "create", "update", "patch", "delete", "deletecollection":
		return true
	}
	return false
}

// ParseRequest derives Kubernetes request information from an HTTP method
// and URL path. It returns ok=false for paths that are not resource paths
// (for example /version or /openapi).
func ParseRequest(method, path string, watch bool) (RequestInfo, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	var info RequestInfo
	switch {
	case len(parts) >= 2 && parts[0] == "api":
		info.APIVersion = parts[1]
		parts = parts[2:]
	case len(parts) >= 3 && parts[0] == "apis":
		info.APIGroup = parts[1]
		info.APIVersion = parts[2]
		parts = parts[3:]
	default:
		return RequestInfo{}, false
	}
	if len(parts) == 0 {
		return RequestInfo{}, false
	}
	// /namespaces/{ns}/{resource}... except the namespace object itself.
	if parts[0] == "namespaces" && len(parts) >= 3 {
		info.Namespace = parts[1]
		parts = parts[2:]
	}
	info.Resource = parts[0]
	if len(parts) >= 2 {
		info.Name = parts[1]
	}
	if len(parts) >= 3 {
		info.Subresource = strings.Join(parts[2:], "/")
	}
	for _, s := range []string{info.APIGroup, info.APIVersion, info.Namespace, info.Resource, info.Name} {
		if len(s) > 253 {
			return RequestInfo{}, false
		}
	}
	switch method {
	case http.MethodPost:
		info.Verb = "create"
	case http.MethodPut:
		info.Verb = "update"
	case http.MethodPatch:
		info.Verb = "patch"
	case http.MethodDelete:
		if info.Name == "" {
			info.Verb = "deletecollection"
		} else {
			info.Verb = "delete"
		}
	case http.MethodGet, http.MethodHead:
		switch {
		case watch:
			info.Verb = "watch"
		case info.Name == "":
			info.Verb = "list"
		default:
			info.Verb = "get"
		}
	default:
		return RequestInfo{}, false
	}
	return info, true
}
