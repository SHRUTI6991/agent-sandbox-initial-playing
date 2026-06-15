// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api_upgrade

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
)

func TestUpgrade(t *testing.T) {
	ctx := t.Context()
	tc := framework.NewTestContext(t)

	// Define the CRDs we need to manage during the upgrade test.
	crdNames := []string{
		"sandboxes.agents.x-k8s.io",
		"sandboxclaims.extensions.agents.x-k8s.io",
		"sandboxtemplates.extensions.agents.x-k8s.io",
		"sandboxwarmpools.extensions.agents.x-k8s.io",
	}

	// Wait for the existing controller deployment to be Ready (so we know it has patched the CRDs with valid certs).
	t.Log("Waiting for existing controller to be ready to ensure CRDs have valid caBundle...")
	require.Eventually(t, func() bool {
		d := &appsv1.Deployment{}
		if err := tc.Get(ctx, types.NamespacedName{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}, d); err != nil {
			return false
		}
		if d.Status.ReadyReplicas == 0 {
			return false
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := tc.Get(ctx, types.NamespacedName{Name: "sandboxes.agents.x-k8s.io"}, crd); err != nil {
			return false
		}
		if crd.Spec.Conversion == nil || crd.Spec.Conversion.Webhook == nil || crd.Spec.Conversion.Webhook.ClientConfig == nil {
			return false
		}
		ca := crd.Spec.Conversion.Webhook.ClientConfig.CABundle
		if len(ca) <= 10 { // Cg== (placeholder) is 1 byte, real cert is >10 bytes
			return false
		}
		return true
	}, 1*time.Minute, 1*time.Second)

	// 1. Fetch and back up original CRD definitions and controller deployment replica count.
	originalCRDs := make(map[string]*apiextensionsv1.CustomResourceDefinition)
	for _, name := range crdNames {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err := tc.Get(ctx, types.NamespacedName{Name: name}, crd)
		require.NoError(t, err)
		originalCRDs[name] = crd.DeepCopy()
	}

	deploy := &appsv1.Deployment{}
	err := tc.Get(ctx, types.NamespacedName{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}, deploy)
	require.NoError(t, err)
	require.NotNil(t, deploy.Spec.Replicas)
	originalReplicas := *deploy.Spec.Replicas

	// Register a robust cleanup to restore original CRDs and scale back the controller.
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Log("Restoring original CRD definitions and controller deployment...")

		// Restore controller deployment replicas
		d := &appsv1.Deployment{}
		if err := tc.Get(cleanupCtx, types.NamespacedName{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}, d); err == nil {
			if d.Spec.Replicas == nil || *d.Spec.Replicas != originalReplicas {
				d.Spec.Replicas = &originalReplicas
				if err := tc.Update(cleanupCtx, d); err != nil {
					t.Logf("cleanup warning: failed to restore controller replicas: %v", err)
				}
			}
		}

		// Restore CRD definitions
		for name, originalCRD := range originalCRDs {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := tc.Get(cleanupCtx, types.NamespacedName{Name: name}, crd); err == nil {
				crd.Spec = originalCRD.Spec
				if err := tc.Update(cleanupCtx, crd); err != nil {
					t.Logf("cleanup warning: failed to restore CRD %s: %v", name, err)
				}
			}
		}
	})

	// --- PHASE 1: Install the previous version (demote CRDs to v1alpha1 storage version) ---
	t.Log("Phase 1: Scaling down current controller and demoting CRDs to v1alpha1 storage version...")

	// Scale down current controller so it does not interfere during the v1alpha1 state.
	zero := int32(0)
	deploy.Spec.Replicas = &zero
	err = tc.Update(ctx, deploy)
	require.NoError(t, err)

	// Wait for the controller pods to terminate.
	require.Eventually(t, func() bool {
		pods := &corev1.PodList{}
		err := tc.List(ctx, pods, client.InNamespace("agent-sandbox-system"), client.MatchingLabels{"app": "agent-sandbox-controller"})
		if err != nil {
			return false
		}
		return len(pods.Items) == 0
	}, 1*time.Minute, 2*time.Second, "expected controller pods to terminate")

	// Update CRD specs to set v1alpha1 as the storage version and disable conversion webhook.
	for _, name := range crdNames {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err := tc.Get(ctx, types.NamespacedName{Name: name}, crd)
		require.NoError(t, err)

		for i, v := range crd.Spec.Versions {
			switch v.Name {
			case "v1alpha1":
				crd.Spec.Versions[i].Served = true
				crd.Spec.Versions[i].Storage = true
			case "v1beta1":
				crd.Spec.Versions[i].Served = false
				crd.Spec.Versions[i].Storage = false
			}
		}

		crd.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{
			Strategy: apiextensionsv1.NoneConverter,
		}

		err = tc.Update(ctx, crd)
		require.NoError(t, err)
	}

	// Wait for CRD schemas to be fully established and discovery updated for v1alpha1.
	for name := range originalCRDs {
		require.Eventually(t, func() bool {
			var list client.ObjectList
			switch name {
			case "sandboxes.agents.x-k8s.io":
				list = &sandboxv1alpha1.SandboxList{}
			case "sandboxtemplates.extensions.agents.x-k8s.io":
				list = &extensionsv1alpha1.SandboxTemplateList{}
			case "sandboxclaims.extensions.agents.x-k8s.io":
				list = &extensionsv1alpha1.SandboxClaimList{}
			case "sandboxwarmpools.extensions.agents.x-k8s.io":
				list = &extensionsv1alpha1.SandboxWarmPoolList{}
			}
			err := tc.List(ctx, list)
			if err == nil {
				return true
			}
			t.Logf("Waiting for CRD %s to serve v1alpha1: %v", name, err)
			return false
		}, 1*time.Minute, 500*time.Millisecond)
	}

	// Set up a unique namespace for this e2e test run.
	nsName := fmt.Sprintf("sandbox-upgrade-test-%d", time.Now().UnixNano())
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
		},
	}
	err = tc.CreateWithCleanup(ctx, ns)
	require.NoError(t, err)

	t.Logf("Creating v1alpha1 resources in namespace %s...", nsName)

	// Create SandboxTemplate in v1alpha1.
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "upgrade-template",
			Namespace: nsName,
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "pause",
							Image: "registry.k8s.io/pause:3.10",
						},
					},
				},
			},
		},
	}
	err = tc.CreateWithCleanup(ctx, template)
	require.NoError(t, err)

	// Create SandboxWarmPool in v1alpha1.
	pool := &extensionsv1alpha1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "upgrade-pool",
			Namespace: nsName,
		},
		Spec: extensionsv1alpha1.SandboxWarmPoolSpec{
			Replicas: 1,
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
				Name: "upgrade-template",
			},
		},
	}
	err = tc.CreateWithCleanup(ctx, pool)
	require.NoError(t, err)

	// Create a cold-start SandboxClaim in v1alpha1.
	wpDefault := extensionsv1alpha1.WarmPoolPolicyDefault
	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "upgrade-claim",
			Namespace: nsName,
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
				Name: "upgrade-template",
			},
			WarmPool: &wpDefault,
		},
	}
	err = tc.CreateWithCleanup(ctx, claim)
	require.NoError(t, err)

	// Create a SandboxClaim in v1alpha1 referencing a specific warm pool.
	wpSpecific := extensionsv1alpha1.WarmPoolPolicy("upgrade-pool")
	claimSpecific := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "upgrade-claim-specific",
			Namespace: nsName,
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
				Name: "upgrade-template",
			},
			WarmPool: &wpSpecific,
		},
	}
	err = tc.CreateWithCleanup(ctx, claimSpecific)
	require.NoError(t, err)

	// Create a Sandbox in v1alpha1 with replicas: 0 (which will convert to operatingMode: Suspended).
	replicas := int32(0)
	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "upgrade-sandbox",
			Namespace: nsName,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas: &replicas,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "pause",
							Image: "registry.k8s.io/pause:3.10",
						},
					},
				},
			},
		},
	}
	err = tc.CreateWithCleanup(ctx, sandbox)
	require.NoError(t, err)

	// --- PHASE 2: Trigger the automated bootstrap job ---
	t.Log("Phase 2: Triggering automated bootstrap phase of the migration script...")

	kubeconfig := framework.GetKubeconfig()
	repoRoot := filepath.Dir(filepath.Dir(kubeconfig))
	migrateScriptPath := filepath.Join(repoRoot, "dev/tools/migrate.sh")

	runMigrationScript := func(phase string) {
		cmd := exec.CommandContext(ctx, "bash", migrateScriptPath, "--phase="+phase, "--namespace="+nsName)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+framework.GetKubeconfig())
		output, err := cmd.CombinedOutput()
		t.Logf("Migration script (%s) output:\n%s", phase, string(output))
		require.NoError(t, err, "failed to run migration script for phase %s", phase)
	}

	runMigrationScript("bootstrap")

	// Verify that the bootstrap script successfully pre-created the shadow pool for our cold-start claim.
	shadowPoolName := "shadow-pool-upgrade-template"
	shadowPool := &extensionsv1alpha1.SandboxWarmPool{}
	err = tc.Get(ctx, types.NamespacedName{Name: shadowPoolName, Namespace: nsName}, shadowPool)
	require.NoError(t, err, "expected shadow pool to be pre-created by bootstrap phase")
	require.Equal(t, int32(0), shadowPool.Spec.Replicas)
	require.Equal(t, "upgrade-template", shadowPool.Spec.TemplateRef.Name)

	// --- PHASE 3: Upgrade (Apply new CRDs and deploy new controller manager) ---
	t.Log("Phase 3: Restoring CRD definitions to v1beta1 storage version and starting the controller...")

	// Restore CRDs back to their original definitions (v1beta1 storage version, conversion webhook active).
	for name, originalCRD := range originalCRDs {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err := tc.Get(ctx, types.NamespacedName{Name: name}, crd)
		require.NoError(t, err)

		crd.Spec = originalCRD.Spec
		err = tc.Update(ctx, crd)
		require.NoError(t, err)
	}

	// Wait for CRD schemas to serve v1beta1 in their Spec.
	for name := range originalCRDs {
		require.Eventually(t, func() bool {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := tc.Get(ctx, types.NamespacedName{Name: name}, crd); err != nil {
				return false
			}
			for _, v := range crd.Spec.Versions {
				if v.Name == "v1beta1" && v.Served {
					return true
				}
			}
			return false
		}, 1*time.Minute, 500*time.Millisecond)
	}

	// Scale up the controller deployment back to its original replicas.
	d := &appsv1.Deployment{}
	err = tc.Get(ctx, types.NamespacedName{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}, d)
	require.NoError(t, err)
	d.Spec.Replicas = &originalReplicas
	err = tc.Update(ctx, d)
	require.NoError(t, err)

	// Wait for the controller deployment to be fully Ready.
	require.Eventually(t, func() bool {
		currentDeploy := &appsv1.Deployment{}
		err := tc.Get(ctx, types.NamespacedName{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}, currentDeploy)
		if err != nil {
			return false
		}
		return currentDeploy.Status.ReadyReplicas == originalReplicas
	}, 2*time.Minute, 2*time.Second, "expected controller deployment to become ready")

	// Wait for the webhook Service endpoints (EndpointSlices) to be populated and ready.
	require.Eventually(t, func() bool {
		slices := &discoveryv1.EndpointSliceList{}
		err := tc.List(ctx, slices, client.InNamespace("agent-sandbox-system"), client.MatchingLabels{"kubernetes.io/service-name": "agent-sandbox-webhook-service"})
		if err != nil {
			return false
		}
		for _, slice := range slices.Items {
			for _, ep := range slice.Endpoints {
				if len(ep.Addresses) > 0 && ep.Conditions.Ready != nil && *ep.Conditions.Ready {
					return true
				}
			}
		}
		return false
	}, 1*time.Minute, 2*time.Second, "expected webhook service endpoint slices to be populated and ready")

	// Wait for the conversion webhook to be fully ready and accepting requests.
	// We do this by attempting to list SandboxWarmPools as v1beta1. Since the existing
	// resources are stored in v1alpha1, the API server must call the conversion webhook
	// to return them as v1beta1. Once this succeeds, the webhook is fully ready.
	t.Log("Waiting for conversion webhook to be ready to handle requests...")
	require.Eventually(t, func() bool {
		pools := &extensionsv1beta1.SandboxWarmPoolList{}
		err := tc.List(ctx, pools, client.InNamespace(nsName))
		if err != nil {
			t.Logf("Webhook not ready yet (will retry): %v", err)
			return false
		}
		return true
	}, 1*time.Minute, 2*time.Second, "expected conversion webhook to become ready")

	// --- PHASE 4: Trigger the automated migration job ---
	t.Log("Phase 4: Running automated migration phase of the migration script...")
	runMigrationScript("migrate")

	// --- PHASE 5: Validation ---
	t.Log("Phase 5: Validating migrated resources and conversion webhook...")

	// Verify Sandbox is converted correctly (replicas: 0 -> operatingMode: Suspended).
	sbBeta := &sandboxv1beta1.Sandbox{}
	err = tc.Get(ctx, types.NamespacedName{Name: "upgrade-sandbox", Namespace: nsName}, sbBeta)
	require.NoError(t, err)
	require.Equal(t, sandboxv1beta1.SandboxOperatingModeSuspended, sbBeta.Spec.OperatingMode)
	require.Contains(t, sbBeta.Annotations, "agents.x-k8s.io/storage-migrated-at")

	// Verify SandboxClaim is converted correctly (warmpool: "default" cold-start -> warmPoolRef.name: "shadow-pool-upgrade-template").
	claimBeta := &extensionsv1beta1.SandboxClaim{}
	err = tc.Get(ctx, types.NamespacedName{Name: "upgrade-claim", Namespace: nsName}, claimBeta)
	require.NoError(t, err)
	require.Equal(t, "shadow-pool-upgrade-template", claimBeta.Spec.WarmPoolRef.Name)
	require.Contains(t, claimBeta.Annotations, "agents.x-k8s.io/storage-migrated-at")

	// Verify SandboxClaim with specific warm pool is converted correctly (warmpool: "upgrade-pool" -> warmPoolRef.name: "upgrade-pool").
	claimSpecificBeta := &extensionsv1beta1.SandboxClaim{}
	err = tc.Get(ctx, types.NamespacedName{Name: "upgrade-claim-specific", Namespace: nsName}, claimSpecificBeta)
	require.NoError(t, err)
	require.Equal(t, "upgrade-pool", claimSpecificBeta.Spec.WarmPoolRef.Name)
	require.Contains(t, claimSpecificBeta.Annotations, "agents.x-k8s.io/storage-migrated-at")

	// Verify SandboxTemplate was migrated.
	templateBeta := &extensionsv1beta1.SandboxTemplate{}
	err = tc.Get(ctx, types.NamespacedName{Name: "upgrade-template", Namespace: nsName}, templateBeta)
	require.NoError(t, err)
	require.Contains(t, templateBeta.Annotations, "agents.x-k8s.io/storage-migrated-at")

	// Verify SandboxWarmPool was migrated.
	poolBeta := &extensionsv1beta1.SandboxWarmPool{}
	err = tc.Get(ctx, types.NamespacedName{Name: "upgrade-pool", Namespace: nsName}, poolBeta)
	require.NoError(t, err)
	require.Contains(t, poolBeta.Annotations, "agents.x-k8s.io/storage-migrated-at")

	// Verify that the resources can still be read successfully in v1alpha1 format via the conversion webhook.
	sbAlpha := &sandboxv1alpha1.Sandbox{}
	err = tc.Get(ctx, types.NamespacedName{Name: "upgrade-sandbox", Namespace: nsName}, sbAlpha)
	require.NoError(t, err)
	require.NotNil(t, sbAlpha.Spec.Replicas)
	require.Equal(t, int32(0), *sbAlpha.Spec.Replicas)

	claimAlpha := &extensionsv1alpha1.SandboxClaim{}
	err = tc.Get(ctx, types.NamespacedName{Name: "upgrade-claim-specific", Namespace: nsName}, claimAlpha)
	require.NoError(t, err)
	require.NotNil(t, claimAlpha.Spec.WarmPool)
	require.Equal(t, extensionsv1alpha1.WarmPoolPolicy("upgrade-pool"), *claimAlpha.Spec.WarmPool)

	// Verify a new v1beta1 resource can coexist and be created successfully.
	newClaim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "new-beta-claim",
			Namespace: nsName,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{
				Name: "upgrade-pool",
			},
		},
	}
	err = tc.CreateWithCleanup(ctx, newClaim)
	require.NoError(t, err)

	t.Log("E2E upgrade test completed successfully!")
}
