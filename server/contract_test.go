package server

import (
	"bufio"
	"bytes"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.vicoworks.com/open-persona/api"
)

var (
	openAPIPath   = regexp.MustCompile(`^  (/\S*):\s*$`)
	openAPIMethod = regexp.MustCompile(`^    (get|post|put|patch|delete):\s*$`)
	pathParam     = regexp.MustCompile(`\{[^}]+\}`)
	openAPICodes  = regexp.MustCompile(`^\s+enum: \[(invalid_argument[^\]]*)\]`)
)

// openAPIOperations lists "METHOD /path" for every operation in the
// document, with path parameters normalized to {}.
func openAPIOperations(t *testing.T) []string {
	t.Helper()
	var ops []string
	path := ""
	sc := bufio.NewScanner(bytes.NewReader(api.OpenAPI))
	for sc.Scan() {
		line := sc.Text()
		if m := openAPIPath.FindStringSubmatch(line); m != nil {
			path = m[1]
		} else if strings.HasPrefix(line, "components:") {
			break
		} else if m := openAPIMethod.FindStringSubmatch(line); m != nil && path != "" {
			ops = append(ops, strings.ToUpper(m[1])+" "+pathParam.ReplaceAllString(path, "{}"))
		}
	}
	slices.Sort(ops)
	return ops
}

// The HTTP API serves exactly the operations api/openapi.yaml documents.
func TestRoutesMatchOpenAPI(t *testing.T) {
	var served []string
	for _, rt := range (&Server{}).routes() {
		served = append(served, pathParam.ReplaceAllString(rt.pattern, "{}"))
	}
	slices.Sort(served)
	documented := openAPIOperations(t)
	if !slices.Equal(served, documented) {
		t.Fatalf("routes and api/openapi.yaml differ\nserved:     %v\ndocumented: %v", served, documented)
	}
}

// Every error code is documented, and nothing else.
func TestErrorCodesMatchOpenAPI(t *testing.T) {
	codes := []Code{CodeInvalidArgument, CodeUnauthenticated, CodePermissionDenied, CodeNotFound,
		CodeUnavailable, CodeUnimplemented, CodeUpstream, CodeInternal}
	var want []string
	for _, c := range codes {
		want = append(want, string(c))
	}
	slices.Sort(want)
	var documented []string
	sc := bufio.NewScanner(bytes.NewReader(api.OpenAPI))
	for sc.Scan() {
		if m := openAPICodes.FindStringSubmatch(sc.Text()); m != nil {
			for _, c := range strings.Split(m[1], ",") {
				documented = append(documented, strings.TrimSpace(c))
			}
		}
	}
	slices.Sort(documented)
	if !slices.Equal(want, documented) {
		t.Fatalf("error codes: code has %v, api/openapi.yaml has %v", want, documented)
	}
}
