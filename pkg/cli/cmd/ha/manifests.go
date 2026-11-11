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
	"fmt"
	"strings"

	operatorv1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/operator/pkg/apis/core/v1beta1"
	syncercrd "github.com/gke-labs/kube-etl/syncer/config/crd"
	mclcrd "github.com/gke-labs/multicluster-leader-election/config/crd/bases"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// manifestBuilder constructs and renders the Kubernetes manifests required for multi-cluster HA on a single cluster.
type manifestBuilder struct {
	contextName string
	// gkeCluster is the GKE cluster this Config Connector runs in; its resource name is the
	// cluster's identity in the multi-cluster election.
	gkeCluster     operatorv1beta1.GKECluster
	project        string
	peerProjects   []string
	mode           string
	mclImage       string
	syncerImage    string
	leaseName      string
	leaseNamespace string
	bucket         string
}

type controllerComponent struct {
	deploymentName     string
	componentLabel     string
	serviceAccountName string
	clusterRoleName    string
	policyRules        []rbacv1.PolicyRule
	extraBindings      []*rbacv1.ClusterRoleBinding
	container          corev1.Container
}

func newManifestBuilder(contextName string, gkeCluster operatorv1beta1.GKECluster, project, mode, mclImage, syncerImage, leaseName, leaseNamespace, bucket string) *manifestBuilder {
	return &manifestBuilder{
		contextName:    contextName,
		gkeCluster:     gkeCluster,
		project:        project,
		peerProjects:   []string{project},
		mode:           mode,
		mclImage:       mclImage,
		syncerImage:    syncerImage,
		leaseName:      leaseName,
		leaseNamespace: leaseNamespace,
		bucket:         bucket,
	}
}

func (b *manifestBuilder) setPeerProjects(peerProjects []string) {
	if len(peerProjects) == 0 {
		b.peerProjects = []string{b.project}
		return
	}
	b.peerProjects = peerProjects
}

const (
	mclAPIGroup    = "multicluster.core.cnrm.cloud.google.com"
	syncerAPIGroup = "syncer.gkelabs.io"
	rbacAPIGroup   = "rbac.authorization.k8s.io"

	mclServiceAccountName    = "kcc-mcl"
	syncerServiceAccountName = "kcc-syncer"

	mclClusterRoleName    = "cnrm-mcl-controller"
	syncerClusterRoleName = "cnrm-syncer-controller"

	syncerAdminBindingName = "cnrm-syncer-admin"
	syncerPeersBindingName = "cnrm-syncer-peers"
	cnrmAdminClusterRole   = "cnrm-admin"
	cnrmViewerClusterRole  = "cnrm-viewer"

	mclDeploymentName    = "multiclusterlease-controller-manager"
	syncerDeploymentName = "syncer-controller-manager"

	syncerDefaultSyncPeriodArg = "--sync-period=1h"
)

func (b *manifestBuilder) buildClusterObjects() []client.Object {
	mclComponent := controllerComponent{
		deploymentName:     mclDeploymentName,
		componentLabel:     "mcl-controller",
		serviceAccountName: mclServiceAccountName,
		clusterRoleName:    mclClusterRoleName,
		policyRules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{mclAPIGroup},
				Resources: []string{"multiclusterleases"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{mclAPIGroup},
				Resources: []string{"multiclusterleases/status"},
				Verbs:     []string{"get", "update", "patch"},
			},
			{
				APIGroups: []string{mclAPIGroup},
				Resources: []string{"multiclusterleases/finalizers"},
				Verbs:     []string{"update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"create", "patch"},
			},
		},
		container: corev1.Container{
			Name:  "manager",
			Image: b.mclImage,
			Args:  []string{fmt.Sprintf("--gcs-bucket=%s", b.bucket)},
		},
	}

	syncerComponent := controllerComponent{
		deploymentName:     syncerDeploymentName,
		componentLabel:     "syncer-controller",
		serviceAccountName: syncerServiceAccountName,
		clusterRoleName:    syncerClusterRoleName,
		policyRules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{syncerAPIGroup},
				Resources: []string{"krmsyncers"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{syncerAPIGroup},
				Resources: []string{"krmsyncers/status"},
				Verbs:     []string{"get", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"namespaces"},
				Verbs:     []string{"get", "list", "watch", "create"},
			},
		},
		extraBindings: []*rbacv1.ClusterRoleBinding{
			b.buildSyncerAdminClusterRoleBinding(),
			b.buildSyncerPeersClusterRoleBinding(),
		},
		container: corev1.Container{
			Name:  "manager",
			Image: b.syncerImage,
			Args:  []string{syncerDefaultSyncPeriodArg},
		},
	}

	objs := make([]client.Object, 0, 14)
	objs = append(objs, buildCRDObjects()...)
	objs = append(objs, b.buildComponentObjects(mclComponent)...)
	objs = append(objs, b.buildComponentObjects(syncerComponent)...)
	objs = append(objs, b.buildManagerHARBACObjects()...)
	return objs
}

func buildCRDObjects() []client.Object {
	return []client.Object{
		mustUnmarshalCRD(mclcrd.MultiClusterLeaseCRDYAML),
		mustUnmarshalCRD(syncercrd.KRMSyncerCRDYAML),
	}
}

func mustUnmarshalCRD(raw []byte) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(raw, crd); err != nil {
		panic(fmt.Sprintf("failed to unmarshal embedded CRD YAML: %v", err))
	}
	return crd
}

const (
	manifestSystemNamespace   = "cnrm-system"
	manifestSystemSAGroup     = "system:serviceaccounts:cnrm-system"
	workloadIdentityGSAAnnot  = "iam.gke.io/gcp-service-account"
	manifestComponentLabelKey = "cnrm.cloud.google.com/component"
)

func (b *manifestBuilder) buildSyncerAdminClusterRoleBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: syncerAdminBindingName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacAPIGroup,
			Kind:     "ClusterRole",
			Name:     cnrmAdminClusterRole,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      syncerServiceAccountName,
				Namespace: manifestSystemNamespace,
			},
		},
	}
}

func (b *manifestBuilder) buildSyncerPeersClusterRoleBinding() *rbacv1.ClusterRoleBinding {
	subjects := make([]rbacv1.Subject, 0, len(b.peerProjects))
	for _, peerProj := range b.peerProjects {
		subjects = append(subjects, rbacv1.Subject{
			APIGroup: rbacAPIGroup,
			Kind:     "User",
			Name:     fmt.Sprintf("%s@%s.iam.gserviceaccount.com", syncerServiceAccountName, peerProj),
		})
	}
	return &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: syncerPeersBindingName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacAPIGroup,
			Kind:     "ClusterRole",
			Name:     cnrmViewerClusterRole,
		},
		Subjects: subjects,
	}
}

func (b *manifestBuilder) buildComponentObjects(comp controllerComponent) []client.Object {
	gsaEmail := fmt.Sprintf("%s@%s.iam.gserviceaccount.com", comp.serviceAccountName, b.project)
	podLabels := map[string]string{"app": comp.componentLabel}
	replicas := int32(1)

	sa := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      comp.serviceAccountName,
			Namespace: manifestSystemNamespace,
			Annotations: map[string]string{
				workloadIdentityGSAAnnot: gsaEmail,
			},
		},
	}

	role := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: comp.clusterRoleName},
		Rules:      comp.policyRules,
	}

	binding := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: comp.clusterRoleName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacAPIGroup,
			Kind:     "ClusterRole",
			Name:     comp.clusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      comp.serviceAccountName,
				Namespace: manifestSystemNamespace,
			},
		},
	}

	deployment := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      comp.deploymentName,
			Namespace: manifestSystemNamespace,
			Labels: map[string]string{
				manifestComponentLabelKey: comp.componentLabel,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Selector: &metav1.LabelSelector{MatchLabels: podLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName: comp.serviceAccountName,
					Containers:         []corev1.Container{comp.container},
				},
			},
		},
	}

	objs := make([]client.Object, 0, 4+len(comp.extraBindings))
	objs = append(objs, sa, role, binding)
	for _, extra := range comp.extraBindings {
		objs = append(objs, extra)
	}
	objs = append(objs, deployment)
	return objs
}

const (
	managerHARoleName                   = "cnrm-manager-ha"
	controllerManagerServiceAccountName = "cnrm-controller-manager"
	namespacedMode                      = "namespaced"
)

func (b *manifestBuilder) buildManagerHARBACObjects() []client.Object {
	role := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      managerHARoleName,
			Namespace: b.leaseNamespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{mclAPIGroup},
				Resources: []string{"multiclusterleases"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
			},
			{
				APIGroups: []string{syncerAPIGroup},
				Resources: []string{"krmsyncers"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"create"},
			},
		},
	}

	binding := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      managerHARoleName,
			Namespace: b.leaseNamespace,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacAPIGroup,
			Kind:     "Role",
			Name:     managerHARoleName,
		},
		Subjects: b.managerRoleBindingSubjects(),
	}

	return []client.Object{role, binding}
}

func (b *manifestBuilder) managerRoleBindingSubjects() []rbacv1.Subject {
	if b.mode == namespacedMode {
		return []rbacv1.Subject{
			{
				APIGroup: rbacAPIGroup,
				Kind:     "Group",
				Name:     manifestSystemSAGroup,
			},
		}
	}
	return []rbacv1.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      controllerManagerServiceAccountName,
			Namespace: manifestSystemNamespace,
		},
	}
}

func (b *manifestBuilder) renderClusterYAML(cc *operatorv1beta1.ConfigConnector) (string, error) {
	objs := b.buildClusterObjects()
	objs = append(objs, b.buildRenderedConfigConnector(cc))

	var out strings.Builder
	fmt.Fprintf(&out, "# Cluster context: %s\n", b.contextName)
	for _, obj := range objs {
		raw, err := yaml.Marshal(obj)
		if err != nil {
			return "", fmt.Errorf("marshaling %s %q to YAML: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}

		out.WriteString("---\n")
		out.Write(raw)
	}

	return out.String(), nil
}

const manifestConfigConnectorName = "configconnector.core.cnrm.cloud.google.com"

func (b *manifestBuilder) buildRenderedConfigConnector(cc *operatorv1beta1.ConfigConnector) *operatorv1beta1.ConfigConnector {
	var spec operatorv1beta1.ConfigConnectorSpec
	if cc != nil {
		spec = *cc.Spec.DeepCopy()
	}

	b.configureMultiClusterLease(&spec)
	return &operatorv1beta1.ConfigConnector{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "core.cnrm.cloud.google.com/v1beta1",
			Kind:       "ConfigConnector",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: manifestConfigConnectorName,
		},
		Spec: spec,
	}
}

const defaultReplicationMode = "Status"

func (b *manifestBuilder) configureMultiClusterLease(spec *operatorv1beta1.ConfigConnectorSpec) {
	if spec.Experiments == nil {
		spec.Experiments = &operatorv1beta1.CCExperiments{}
	}
	if spec.Experiments.MultiClusterLease == nil {
		spec.Experiments.MultiClusterLease = &operatorv1beta1.MultiClusterLeaseSpec{}
	}

	mcl := spec.Experiments.MultiClusterLease
	mcl.LeaseName = b.leaseName
	mcl.Namespace = b.leaseNamespace
	// clusterCandidateIdentity is deprecated and rejected by the ConfigConnector CRD; the
	// cluster's identity is now derived from gkeCluster.
	mcl.ClusterCandidateIdentity = ""
	if mcl.GKECluster == nil {
		gkeCluster := b.gkeCluster
		mcl.GKECluster = &gkeCluster
	}
	if mcl.ResourceReplicationMode == "" {
		mcl.ResourceReplicationMode = defaultReplicationMode
	}
}

func (b *manifestBuilder) leaseKey() string {
	return b.leaseNamespace + "/" + b.leaseName
}
