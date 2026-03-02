import httpx, subprocess

class SandboxControlPlaneClient:
    def __init__(self, sandbox_manager_url: str):
        self.sandbox_manager_url = sandbox_manager_url

    def get_token(self, service_account: str = "default", namespace: str = "default") -> str:
        """Uses your local K8s identity to get a one-time token for a service account."""
        cmd = f"kubectl create token {service_account} --namespace {namespace} --duration=1h"
        return subprocess.check_output(cmd.split()).decode().strip()

    def create_sandbox(self, template: str, namespace: str, sa_name: str = "default", sa_namespace: str = "default") -> dict:
        """
        Creates a new sandbox by calling the sandbox-manager.

        Args:
            template: The name of the SandboxTemplate to use.
            namespace: The namespace where the sandbox will be created.
            sa_name: The service account name to generate the auth token for.
            sa_namespace: The namespace of the service account.
        """
        token = self.get_token(service_account=sa_name, namespace=sa_namespace)
        headers = {"Authorization": f"Bearer {token}"}
        payload = {"template": template, "namespace": namespace}

        with httpx.Client() as client:
            r = client.post(f"{self.sandbox_manager_url}/sandboxes", 
                            json=payload,
                            headers=headers)
            r.raise_for_status()
            # The manager returns a dict like: {"id": "sbx-...", "namespace": "..."}
            return r.json()