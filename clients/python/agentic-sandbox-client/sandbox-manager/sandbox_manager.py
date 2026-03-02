import os, uuid, logging
from fastapi import FastAPI, Header, HTTPException, Depends
from pydantic import BaseModel
from kubernetes import client, config

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("sandbox-manager")

app = FastAPI()

GROUP = "extensions.agents.x-k8s.io"
VERSION = "v1alpha1"
PLURAL = "sandboxclaims"

config.load_incluster_config()
k8s_custom = client.CustomObjectsApi()
k8s_auth = client.AuthenticationV1Api()

class CreateReq(BaseModel):
    template: str
    namespace: str  # User now defines where the sandbox goes
    annotations: dict[str, str] = {}

async def verify_token(authorization: str = Header(None)):
    if not authorization or "Bearer " not in authorization:
        raise HTTPException(status_code=401, detail="Missing Token")
    
    token = authorization.split(" ")[1]
    review = client.V1TokenReview(spec=client.V1TokenReviewSpec(token=token))
    res = k8s_auth.create_token_review(review)
    
    if not res.status.authenticated:
        raise HTTPException(status_code=401, detail="Invalid K8s Token")
    
    return res.status.user.username

@app.post("/sandboxes")
async def create(req: CreateReq, username: str = Depends(verify_token)):
    sbx_id = f"sbx-{uuid.uuid4().hex[:6]}"
    
    # Track who requested this via annotations
    full_annotations = {
        "sandbox.auth/created-by": username,
        **req.annotations 
    }

    body = {
        "apiVersion": f"{GROUP}/{VERSION}",
        "kind": "SandboxClaim",
        "metadata": {
            "name": sbx_id,
            "namespace": req.namespace, # Dynamically assigned from request
            "annotations": full_annotations
        },
        "spec": {
            "sandboxTemplateRef": {
                "name": req.template
            }
        }
    }

    try:
        k8s_custom.create_namespaced_custom_object(
            group=GROUP,
            version=VERSION,
            namespace=req.namespace,
            plural=PLURAL,
            body=body
        )
        logger.info(f"User {username} created sandbox {sbx_id} in namespace {req.namespace}")
        return {"id": sbx_id, "namespace": req.namespace}
    except Exception as e:
        logger.error(f"K8s Error: {e}")
        # Return 400 if namespace doesn't exist or permissions are missing
        raise HTTPException(status_code=400, detail=f"Could not create sandbox in namespace '{req.namespace}': {str(e)}")