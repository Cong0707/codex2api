# Documentation Index

## API Documentation

### OpenAPI/Swagger Specification

The complete OpenAPI 3.0.3 specification is available in [`openapi.yaml`](./openapi.yaml).

### Error Codes Reference

See [`errors.md`](./docs/errors.md) for detailed error code documentation.

### API Versioning

This API uses URL path versioning (e.g., `/v1/`). The current version is v1.0.0.

### Authentication

All API endpoints require authentication via Bearer token in the Authorization header:

```
Authorization: Bearer sk-...
```

### Response Formats

#### Success Response

```json
{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "created": 1234567890,
  "model": "gpt-5.4",
  "choices": [...],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 20,
    "total_tokens": 30
  }
}
```

#### Error Response

```json
{
  "error": {
    "code": "invalid_request",
    "message": "Invalid request",
    "type": "invalid_request_error",
    "details": {
      "field": "model",
      "message": "Model 'invalid-model' is not supported"
    }
  }
}
```

### Rate Limiting

Rate limits are returned in response headers:

- `X-RateLimit-Limit`: Maximum requests allowed
- `X-RateLimit-Remaining`: Remaining requests
- `X-RateLimit-Reset`: Unix timestamp when the limit resets

### Supported Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/models` | GET | List available models (root-path alias: `/models`) |
| `/v1/chat/completions` | POST | Create chat completion (root-path alias: `/chat/completions`) |
| `/v1/responses` | POST | Create response (Codex native, root-path alias: `/responses`) |
| `/v1/responses/compact` | POST | Compact compatibility alias (same as `/v1/responses`, root-path alias: `/responses/compact`) |
| `/v1/images/generations` | POST | OpenAI Images-compatible image generation endpoint (root-path alias: `/images/generations`) |
| `/v1/images/edits` | POST | OpenAI Images-compatible image editing endpoint (root-path alias: `/images/edits`) |
| `/health` | GET | Health check |

If your client uses a `base_url` without `/v1`, the same OpenAI-compatible endpoints are also available on the root paths listed above.

### Model Support

Supported models include:
- `gpt-5.5`
- `gpt-5.4`
- `gpt-5.4-mini`
- `gpt-5.3-codex`
- `gpt-image-2`
- `gpt-5`
- `gpt-5-codex`
- `gpt-5-codex-mini`
- `gpt-5.1`, `gpt-5.1-codex`, etc.

The runtime model list can also be synchronized from the official Codex models page via `POST /api/admin/models/sync`.

See the OpenAPI spec or `GET /v1/models` for the complete list currently exposed by the server.
