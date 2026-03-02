import argparse
from k8s_agent_sandbox_ctrl.sandbox_controlplane_client import SandboxControlPlaneClient

def main():
    """
    Initializes the client and creates a new sandbox using command-line arguments.
    """
    parser = argparse.ArgumentParser(description="Create a new agent sandbox.")
    parser.add_argument("--manager-url", default="http://localhost:8000", help="The URL where the sandbox-manager is exposed.")
    parser.add_argument("--template", default="python-sandbox-template", help="The name of the SandboxTemplate to use.")
    parser.add_argument("--namespace", default="default", help="The namespace where the sandbox will be created.")
    parser.add_argument("--sa-name", default="client-user", help="The service account name for client authentication.")
    parser.add_argument("--sa-namespace", default="default", help="The namespace of the service account.")
    
    args = parser.parse_args()

    print(f"Connecting to sandbox-manager at {args.manager_url}...")
    client = SandboxControlPlaneClient(args.manager_url)
    
    print(f"Requesting sandbox with template '{args.template}' in namespace '{args.namespace}'...")
    try:
        result = client.create_sandbox(
            template=args.template,
            namespace=args.namespace,
            sa_name=args.sa_name,
            sa_namespace=args.sa_namespace
        )
        print("Successfully created sandbox!")
        print(f"  ID: {result['id']}")
        print(f"  Namespace: {result['namespace']}")
    except Exception as e:
        print(f"Error creating sandbox: {e}")

if __name__ == "__main__":
    main()
