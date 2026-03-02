# Sandbox Manager

The Sandbox Manager is a lightweight control plane service that facilitates the creation and management of `Sandbox` resources in a Kubernetes cluster. It exposes a simple HTTP API that authenticated clients can use to request new sandboxes based on pre-defined templates.

## Overview

The Sandbox Manager acts as an intermediary between clients (like AI agents or CLI tools) and the Kubernetes API. Instead of giving clients direct permissions to create Custom Resources, they authenticate with the Manager, which then creates `SandboxClaim` resources on their behalf.

### Key Features

*   **Simple API**: RESTful API for creating sandboxes.
*   **Authentication**: Verifies Kubernetes Service Account tokens using the `TokenReview` API.
*   **Namespace Isolation**: Supports creating sandboxes in specific namespaces.
*   **Template-based**: Uses `SandboxTemplate` resources to define sandbox configurations.

## Prerequisites

*   A Kubernetes cluster.
*   The `agent-sandbox` controller and CRDs installed.
*   `kubectl` configured to communicate with your cluster.

## Deployment

### 1. Build the Docker Image

Navigate to the `sandbox-manager` directory and build the image.

```bash
docker build -t your-registry/sandbox-manager:latest .
docker push your-registry/sandbox-manager:latest
```

### 2. Deploy to Kubernetes

Update the `sandbox-manager-deployment.yaml` file with your image name, then apply it:

```bash
kubectl apply -f sandbox-manager-deployment.yaml
```

This will create:
*   A `ServiceAccount` (`sandbox-manager-sa`) with permissions to manage `SandboxClaims` and perform `TokenReviews`.
*   A `Deployment` running the manager.
*   A `Service` exposing the manager on port 80 (target port 8000).

## Usage

### API Endpoints

#### `POST /sandboxes`

Creates a new Sandbox.

**Headers:**
*   `Authorization`: `Bearer <K8S_SERVICE_ACCOUNT_TOKEN>`

**Body (JSON):**
```json
{
  "template": "python-sandbox-template",
  "namespace": "default",
  "annotations": {
    "custom-key": "custom-value"
  }
}
```

**Response:**
```json
{
  "id": "sbx-123456",
  "namespace": "default"
}
```

## Development & Debugging

### Port Forwarding

To access the manager running in the cluster from your local machine:

```bash
kubectl port-forward svc/sandbox-manager-svc 8000:80
```

Then you can send requests to `http://localhost:8000`.
