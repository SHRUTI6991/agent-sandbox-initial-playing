// Copyright 2025 The Kubernetes Authors.
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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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

func TestHelmUpgrade(t *testing.T) {
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

	// Parse image repository and tag from the existing controller.
	fullImage := deploy.Spec.Template.Spec.Containers[0].Image
	parts := strings.Split(fullImage, ":")
	require.Len(t, parts, 2, "unexpected image name format: %s", fullImage)
	imageRepo := parts[0]
	imageTag := parts[1]

	kubeconfig := framework.GetKubeconfig()
	helmPath := filepath.Join(filepath.Dir(kubeconfig), "helm") // bin/helm

	// Register a robust cleanup to uninstall helm release and redeploy using deploy-to-kube.
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Log("Cleanup: Uninstalling helm release and restoring deploy-to-kube deployment...")

		// Uninstall helm release
		uninstallCmd := exec.Command(helmPath, "--kubeconfig", kubeconfig, "uninstall", "agent-sandbox", "-n", "agent-sandbox-system")
		_ = uninstallCmd.Run() // Ignore errors if already uninstalled

		// Delete CRDs to ensure clean slate for deploy-to-kube
		for _, name := range crdNames {
			crd := &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: name},
			}
			_ = tc.Delete(cleanupCtx, crd)
		}

		// Wait for CRDs to be fully deleted before redeploying.
		for _, name := range crdNames {
			_ = tc.WaitForObjectNotFound(cleanupCtx, &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: name},
			})
		}

		// Redeploy using dev/tools/deploy-to-kube
		repoRoot := filepath.Dir(filepath.Dir(kubeconfig))
		deployCmd := exec.Command(filepath.Join(repoRoot, "dev", "tools", "deploy-to-kube"), "--image-prefix=kind.local/", "--extensions")
		deployCmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		if output, err := deployCmd.CombinedOutput(); err != nil {
			t.Logf("cleanup warning: failed to run deploy-to-kube: %v\nOutput:\n%s", err, string(output))
		}
	})

	// 2. Tear down the current deploy-to-kube installation to start with Helm from scratch.
	t.Log("Uninstalling any existing controller objects...")

	// Delete Deployment
	_ = tc.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}})

	// Delete Services
	_ = tc.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}})
	_ = tc.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent-sandbox-webhook-service", Namespace: "agent-sandbox-system"}})

	// Delete ServiceAccount
	_ = tc.Delete(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}})

	// Delete ClusterRoles and bindings
	_ = tc.Delete(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "agent-sandbox-controller"}})
	_ = tc.Delete(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "agent-sandbox-controller-extensions"}})
	_ = tc.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "agent-sandbox-controller"}})
	_ = tc.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "agent-sandbox-controller-extensions"}})

	// Wait for controller pods to terminate.
	require.Eventually(t, func() bool {
		pods := &corev1.PodList{}
		err := tc.List(ctx, pods, client.InNamespace("agent-sandbox-system"), client.MatchingLabels{"app": "agent-sandbox-controller"})
		if err != nil {
			return false
		}
		return len(pods.Items) == 0
	}, 1*time.Minute, 2*time.Second, "expected controller pods to terminate")

	// 3. Install the current chart via Helm with migration disabled.
	t.Log("Installing Helm chart with migration disabled...")
	repoRoot := filepath.Dir(filepath.Dir(kubeconfig))
	chartPath := filepath.Join(repoRoot, "helm")

	installCmd := exec.Command(helmPath, "--kubeconfig", kubeconfig, "upgrade", "--install", "agent-sandbox", chartPath,
		"--namespace", "agent-sandbox-system",
		"--set", "namespace.create=false",
		"--set", "migration.enabled=false",
		"--set", "migration.image=bitnamilegacy/kubectl:1.30",
		"--set", "controller.extensions=true",
		"--set", "image.repository="+imageRepo,
		"--set", "image.tag="+imageTag,
	)
	output, err := installCmd.CombinedOutput()
	require.NoError(t, err, "helm install failed: %s", string(output))

	// Scale down Helm deployment to 0 replicas to prevent webhook interference during old-state setup.
	t.Log("Scaling down Helm controller deployment to 0 replicas...")
	helmDeploy := &appsv1.Deployment{}
	err = tc.Get(ctx, types.NamespacedName{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}, helmDeploy)
	require.NoError(t, err)
	zero := int32(0)
	helmDeploy.Spec.Replicas = &zero
	err = tc.Update(ctx, helmDeploy)
	require.NoError(t, err)

	// Wait for controller pods to terminate.
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

	// 4. Create v1alpha1 resources in a test namespace.
	nsName := "sandbox-helm-upgrade-test-" + fmt.Sprintf("%d", time.Now().UnixNano())
	testNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
		},
	}
	err = tc.CreateWithCleanup(ctx, testNS)
	require.NoError(t, err)

	t.Logf("Creating v1alpha1 resources in namespace %s...", nsName)

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

	// 5. Trigger the Helm Upgrade with migration enabled in a background goroutine.
	// This will first trigger the pre-upgrade hook (bootstrap).
	t.Log("Upgrading Helm chart with migration enabled (running helm upgrade in background)...")
	upgradeCmd := exec.Command(helmPath, "--kubeconfig", kubeconfig, "upgrade", "agent-sandbox", chartPath,
		"--namespace", "agent-sandbox-system",
		"--set", "namespace.create=false",
		"--set", "migration.enabled=true",
		"--set", "migration.image=bitnamilegacy/kubectl:1.30",
		"--set", "controller.extensions=true",
		"--set", "image.repository="+imageRepo,
		"--set", "image.tag="+imageTag,
	)

	type cmdResult struct {
		output []byte
		err    error
	}
	cmdChan := make(chan cmdResult, 1)

	go func() {
		out, err := upgradeCmd.CombinedOutput()
		cmdChan <- cmdResult{output: out, err: err}
	}()

	// 6. Wait for the bootstrap Job to finish successfully. Since the CRDs are still v1alpha1,
	// the bootstrap Job (which reads sandboxclaims) does not need the conversion webhook.
	// We check if the Job succeeded OR if the controller deployment has been scaled up by Helm,
	// which indicates the pre-upgrade hook has finished and Helm began the rollout.
	t.Log("Waiting for bootstrap Job to succeed...")
	require.Eventually(t, func() bool {
		d := &appsv1.Deployment{}
		err := tc.Get(ctx, types.NamespacedName{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}, d)
		if err == nil && d.Spec.Replicas != nil && *d.Spec.Replicas > 0 {
			t.Log("Detected deployment rollout started by Helm (pre-upgrade hook completed).")
			return true
		}

		job := &batchv1.Job{}
		err = tc.Get(ctx, types.NamespacedName{Name: "agent-sandbox-migration-bootstrap", Namespace: "agent-sandbox-system"}, job)
		if err == nil && job.Status.Succeeded > 0 {
			t.Log("Detected bootstrap Job succeeded directly.")
			return true
		}

		return false
	}, 2*time.Minute, 1*time.Second, "expected bootstrap Job to succeed or deployment rollout to start")

	// 7. Immediately restore CRD definitions to v1beta1 spec.
	// Helm has now finished the pre-upgrade hook and will immediately begin rolling out the new
	// controller deployment. Since we restore CRDs to v1beta1, the new controller will start up
	// successfully and serve/watch v1beta1 resources.
	t.Log("Bootstrap Job succeeded! Restoring CRD definitions to v1beta1 spec...")
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

	// 8. Wait for the helm upgrade command to complete successfully.
	t.Log("Waiting for helm upgrade to finish...")
	res := <-cmdChan
	require.NoError(t, res.err, "helm upgrade failed: %s", string(res.output))

	// 6. Validation
	t.Log("Validating migrated resources and conversion webhook...")

	// Verify Sandbox is converted correctly (replicas: 0 -> operatingMode: Suspended).
	sbBeta := &sandboxv1beta1.Sandbox{}
	err = tc.Get(ctx, types.NamespacedName{Name: "upgrade-sandbox", Namespace: nsName}, sbBeta)
	require.NoError(t, err)
	require.Equal(t, sandboxv1beta1.SandboxOperatingModeSuspended, sbBeta.Spec.OperatingMode)
	require.Contains(t, sbBeta.Annotations, "agents.x-k8s.io/storage-migrated-at")

	// Verify SandboxClaims are converted correctly
	claimBeta := &extensionsv1beta1.SandboxClaim{}
	err = tc.Get(ctx, types.NamespacedName{Name: "upgrade-claim", Namespace: nsName}, claimBeta)
	require.NoError(t, err)
	require.Equal(t, "shadow-pool-upgrade-template", claimBeta.Spec.WarmPoolRef.Name)
	require.Contains(t, claimBeta.Annotations, "agents.x-k8s.io/storage-migrated-at")

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

	t.Log("E2E Helm upgrade test completed successfully!")
}
