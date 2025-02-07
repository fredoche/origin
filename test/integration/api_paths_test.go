// +build integration

package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"k8s.io/kubernetes/pkg/api/unversioned"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/util/sets"

	configapi "github.com/openshift/origin/pkg/cmd/server/api"
	testutil "github.com/openshift/origin/test/util"
	testserver "github.com/openshift/origin/test/util/server"
)

func TestRootAPIPaths(t *testing.T) {
	// ExceptionalExpectedCodes are codes that we expect, but are not http.StatusOK.
	// These codes are expected because the response from a GET on our root should
	// expose endpoints for discovery, but will not necessarily expose endpoints that
	// are supported as written - i.e. versioned endpoints or endpoints that need
	// context will 404 with the correct credentials and that is OK.
	ExceptionalExpectedCodes := map[string]int{
		"/logs/": http.StatusNotFound,
	}

	defer testutil.RequireEtcd(t).Terminate(t)
	masterConfig, adminConfigFile, err := testserver.StartTestMaster()
	if err != nil {
		t.Fatalf("unexpected error starting test master: %v", err)
	}

	clientConfig, err := testutil.GetClusterAdminClientConfig(adminConfigFile)
	if err != nil {
		t.Fatalf("unexpected error getting cluster admin client config: %v", err)
	}

	transport, err := kclient.TransportFor(clientConfig)
	if err != nil {
		t.Fatalf("unexpected error getting transport for client config: %v", err)
	}

	rootRequest, err := http.NewRequest("GET", masterConfig.AssetConfig.MasterPublicURL+"/", nil)
	rootRequest.Header.Set("Accept", "*/*")
	rootResponse, err := transport.RoundTrip(rootRequest)
	if err != nil {
		t.Fatalf("unexpected error issuing GET to root path: %v", err)
	}

	var broadcastRootPaths unversioned.RootPaths
	if err := json.NewDecoder(rootResponse.Body).Decode(&broadcastRootPaths); err != nil {
		t.Fatalf("unexpected error decoding root path response: %v", err)
	}
	defer rootResponse.Body.Close()

	// We need to make sure that any APILevels specified in the config are present in the RootPaths, and that
	// any not specified are not
	expectedOpenShiftAPILevels := sets.NewString(masterConfig.APILevels...)
	expectedKubeAPILevels := sets.NewString(configapi.GetEnabledAPIVersionsForGroup(*masterConfig.KubernetesMasterConfig, configapi.APIGroupKube)...)
	actualOpenShiftAPILevels := sets.String{}
	actualKubeAPILevels := sets.String{}
	for _, route := range broadcastRootPaths.Paths {
		if strings.HasPrefix(route, "/oapi/") {
			actualOpenShiftAPILevels.Insert(route[6:])
		}

		if strings.HasPrefix(route, "/api/") {
			actualKubeAPILevels.Insert(route[5:])
		}
	}
	if !expectedOpenShiftAPILevels.Equal(actualOpenShiftAPILevels) {
		t.Errorf("actual OpenShift API levels served don't match expected levels:\n\texpected:\n\t%s\n\tgot:\n\t%s", expectedOpenShiftAPILevels.List(), actualOpenShiftAPILevels.List())
	}
	if !expectedKubeAPILevels.Equal(actualKubeAPILevels) {
		t.Errorf("actual Kube API levels served don't match expected levels:\n\texpected:\n\t%s\n\tgot:\n\t%s", expectedKubeAPILevels.List(), actualKubeAPILevels.List())
	}

	// Send a GET to every advertised address and check that we get the correct response
	for _, route := range broadcastRootPaths.Paths {
		req, err := http.NewRequest("GET", masterConfig.AssetConfig.MasterPublicURL+route, nil)
		req.Header.Set("Accept", "*/*")
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Errorf("unexpected error issuing GET for path %q: %v", route, err)
			continue
		}
		// Look up expected code if exceptional or default to 200
		expectedCode, exists := ExceptionalExpectedCodes[route]
		if !exists {
			expectedCode = http.StatusOK
		}
		if resp.StatusCode != expectedCode {
			t.Errorf("incorrect status code for %s endpoint: expected %d, got %d", route, expectedCode, resp.StatusCode)
		}
	}
}
