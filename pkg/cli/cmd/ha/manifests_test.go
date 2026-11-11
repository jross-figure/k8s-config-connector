// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ha

import (
	"os"
	"path/filepath"
	"testing"

	operatorv1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/operator/pkg/apis/core/v1beta1"
	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRenderClusterYAMLGolden(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		goldenPath string
	}{
		{
			name:       "cluster mode",
			mode:       "cluster",
			goldenPath: filepath.Join("testdata", "cluster.golden.yaml"),
		},
		{
			name:       "namespaced mode",
			mode:       "namespaced",
			goldenPath: filepath.Join("testdata", "namespaced.golden.yaml"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gkeCluster := operatorv1beta1.GKECluster{
				Project:  "proj-a",
				Location: "us-central1",
				Name:     "cluster-a",
			}
			builder := newManifestBuilder(
				"gke_proj-a_us-central1_cluster-a",
				gkeCluster,
				"proj-a",
				tc.mode,
				testMCLImage,
				testSyncerImage,
				"kcc-multicluster-lease",
				"cnrm-system",
				"my-ha-bucket",
			)
			builder.setPeerProjects([]string{"proj-b"})
			cc := &operatorv1beta1.ConfigConnector{
				ObjectMeta: metav1.ObjectMeta{
					Name: "configconnector.core.cnrm.cloud.google.com",
				},
				Spec: operatorv1beta1.ConfigConnectorSpec{
					Mode: tc.mode,
				},
			}

			got, err := builder.renderClusterYAML(cc)
			if err != nil {
				t.Fatalf("renderClusterYAML failed: %v", err)
			}

			compareOrWriteGoldenFile(t, tc.goldenPath, got)
		})
	}
}

func compareOrWriteGoldenFile(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("WRITE_GOLDEN_OUTPUT") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("creating golden directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0644); err != nil {
			t.Fatalf("writing golden file %q: %v", path, err)
		}
	}
	wantBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file %q: %v (run with WRITE_GOLDEN_OUTPUT=1 to update)", path, err)
	}
	if diff := cmp.Diff(string(wantBytes), got); diff != "" {
		t.Fatalf("golden file %q mismatch (-want +got):\n%s", path, diff)
	}
}

func TestBuildClusterObjectsOrderingAndInvariants(t *testing.T) {
	gkeCluster := operatorv1beta1.GKECluster{
		Project:  "proj-a",
		Location: "us-central1",
		Name:     "cluster-a",
	}
	builder := newManifestBuilder(
		"gke_proj-a_us-central1_cluster-a",
		gkeCluster,
		"proj-a",
		"cluster",
		testMCLImage,
		testSyncerImage,
		"kcc-multicluster-lease",
		"cnrm-system",
		"my-ha-bucket",
	)
	builder.setPeerProjects([]string{"proj-b", "proj-c"})

	objs := builder.buildClusterObjects()

	if len(objs) != 14 {
		t.Fatalf("got %d objects, want 14", len(objs))
	}
	// First two objects must be the CRDs so subsequent resources or controllers can use them.
	for i, wantCRDName := range []string{
		"multiclusterleases.multicluster.core.cnrm.cloud.google.com",
		"krmsyncers.syncer.gkelabs.io",
	} {
		crd, ok := objs[i].(*apiextensionsv1.CustomResourceDefinition)
		if !ok {
			t.Fatalf("objs[%d] is %T, want *apiextensionsv1.CustomResourceDefinition", i, objs[i])
		}
		if crd.Name != wantCRDName {
			t.Fatalf("objs[%d].Name = %q, want %q", i, crd.Name, wantCRDName)
		}
	}

	var mclDeploy, syncerDeploy *appsv1.Deployment
	var adminBinding, peersBinding *rbacv1.ClusterRoleBinding
	for _, obj := range objs {
		switch typed := obj.(type) {
		case *appsv1.Deployment:
			switch typed.Name {
			case mclDeploymentName:
				mclDeploy = typed
			case syncerDeploymentName:
				syncerDeploy = typed
			}
		case *rbacv1.ClusterRoleBinding:
			switch typed.Name {
			case syncerAdminBindingName:
				adminBinding = typed
			case syncerPeersBindingName:
				peersBinding = typed
			}
		}
	}

	if mclDeploy == nil || syncerDeploy == nil {
		t.Fatalf("missing deployments: mcl=%v, syncer=%v", mclDeploy, syncerDeploy)
	}
	for _, d := range []*appsv1.Deployment{mclDeploy, syncerDeploy} {
		if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
			t.Fatalf("deployment %q replicas = %v, want 1", d.Name, d.Spec.Replicas)
		}
		if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
			t.Fatalf("deployment %q strategy = %q, want Recreate", d.Name, d.Spec.Strategy.Type)
		}
		if len(d.Spec.Template.Spec.Containers[0].Command) != 0 {
			t.Fatalf("deployment %q should use image default ENTRYPOINT without Command override, got %v", d.Name, d.Spec.Template.Spec.Containers[0].Command)
		}
	}
	if got := mclDeploy.Spec.Template.Spec.Containers[0].Args; len(got) != 1 || got[0] != "--gcs-bucket=my-ha-bucket" {
		t.Fatalf("mcl args = %v, want [--gcs-bucket=my-ha-bucket]", got)
	}
	if got := syncerDeploy.Spec.Template.Spec.Containers[0].Args; len(got) != 1 || got[0] != "--sync-period=1h" {
		t.Fatalf("syncer args = %v, want [--sync-period=1h]", got)
	}

	if adminBinding == nil || adminBinding.RoleRef.Name != "cnrm-admin" {
		t.Fatalf("expected cnrm-syncer-admin binding to cnrm-admin, got %+v", adminBinding)
	}
	if peersBinding == nil || peersBinding.RoleRef.Name != "cnrm-viewer" || len(peersBinding.Subjects) != 2 {
		t.Fatalf("expected cnrm-syncer-peers binding to cnrm-viewer with 2 peer subjects, got %+v", peersBinding)
	}
}
