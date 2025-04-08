# ext_proc for LLM prompt & response risk assessment

## Summary

An Envoy ext_proc filter for assessing LLM prompts & responses for risk by
calling a risk assessment LLM like granite-guardian.

## Instructions

You will need the granite-guardian model running somewhere first.
Here is an example kserve InferenceService to deploy a version of it

```shell
kubectl apply -f - <<EOF
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: huggingface-granite-guardian
  namespace: default
spec:
  predictor:
    model:
      modelFormat:
        name: huggingface
      args:
        - --model_name=granite-guardian
        - --model_id=ibm-granite/granite-guardian-3.1-2b
        - --dtype=half
        - --max_model_len=8192
      env:
        - name: HF_TOKEN
          valueFrom:
            secretKeyRef:
              name: hf-secret
              key: HF_TOKEN
              optional: false
      resources:
        limits:
          nvidia.com/gpu: "1"
          cpu: "4"
          memory: 8Gi
        requests:
          cpu: "1"
          memory: 2Gi
EOF
```

Steps to run the ext_proc server locally for use with a local kind cluster, as setup with https://github.com/Kuadrant/kserve-poc

```shell
docker build -t guardian-ext-proc .
docker run -e GUARDIAN_API_KEY=test -e GUARDIAN_URL=http://example.com -p 50051:50051 guardian-ext-proc

kubectl apply -f filter.yaml

GATEWAY_HOST=$(kubectl get gateway -n kserve kserve-ingress-gateway -o jsonpath='{.status.addresses[0].value}')
SERVICE_HOSTNAME=$(kubectl get inferenceservice huggingface-llm -o jsonpath='{.status.url}' | cut -d "/" -f 3)

# OK Prompt

curl -v http://$GATEWAY_HOST/openai/v1/completions \
   -H "content-type: application/json" \
   -H "Host: $SERVICE_HOSTNAME" \
   -d '{"model": "llm", "prompt": "What is Kubernetes", "stream": false, "max_tokens": 10}'
*   Trying 192.168.97.4:80...
* Connected to 192.168.97.4 (192.168.97.4) port 80
> POST /openai/v1/completions HTTP/1.1
> Host: huggingface-llm-default.example.com
> User-Agent: Mozilla/5.0 (compatible; MSIE 9.0; Windows NT 6.1; Trident/5.0)
> Accept: */*
> content-type: application/json
> Content-Length: 83
>
* upload completely sent off: 83 bytes
< HTTP/1.1 200 OK
< date: Mon, 07 Apr 2025 16:07:18 GMT
< server: istio-envoy
< content-length: 387
< content-type: application/json
< x-envoy-upstream-service-time: 13496
<
* Connection #0 to host 192.168.97.4 left intact
{"id":"07d40370-6b1d-4989-b8eb-cacd02846ef4","object":"text_completion","created":1744042053,"model":"llm","choices":[{"index":0,"text":"?\n\nKubernetes is a container orchestr","logprobs":null,"finish_reason":"length","stop_reason":null,"prompt_logprobs":null}],"usage":{"prompt_tokens":5,"total_tokens":15,"completion_tokens":10,"prompt_tokens_details":null},"system_fingerprint":null}


# Not OK/Risky Prompt

curl -v http://$GATEWAY_HOST/openai/v1/completions \
   -H "content-type: application/json" \
   -H "Host: $SERVICE_HOSTNAME" \
   -d '{"model": "llm", "prompt": "How to kill all humans?", "stream": false, "max_tokens": 10}'
*   Trying 192.168.97.4:80...
* Connected to 192.168.97.4 (192.168.97.4) port 80
> POST /openai/v1/completions HTTP/1.1
> Host: huggingface-llm-default.example.com
> User-Agent: Mozilla/5.0 (compatible; MSIE 9.0; Windows NT 6.1; Trident/5.0)
> Accept: */*
> content-type: application/json
> Content-Length: 88
>
* upload completely sent off: 88 bytes
< HTTP/1.1 403 Forbidden
< content-type:
< content-length: 44
< date: Mon, 07 Apr 2025 16:08:32 GMT
< server: istio-envoy
<
* Connection #0 to host 192.168.97.4 left intact
{"error":"Prompt blocked by content policy"}
```

## Optional Env Vars

* `DISABLE_PROMPT_RISK_CHECK` - If set to "yes", skips risk checks on prompts
* `DISABLE_RESPONSE_RISK_CHECK` - If set to "yes", skips risk checks on responses

If unset, both prompt and response risk checks are active.
