# Simple Storage Service

S3 substitute for local storage. Meant to be used as an API behind a safe service. Not meant to be:

- A full substitute to file storages, such as MINIO.
- Fully secure with TLS, folder mounting constraints, etc.
- Replacement for file storages

Can be used:

- Along with nginx with proper configuration to serve static files
- Along with a secure handler to properly update files
- Simple servers with local storage

## Features

*   **S3-Compatible API**: Implements basic `PUT`, `GET`, and `DELETE` operations using `/{bucket}/{key}` routing.
*   **Bucket Listing**: `GET /list` endpoint to retrieve all buckets and file metadata (size, content type, modification time).

## Configuration

The service is configured via environment variables:

| Variable | Description | Default |
| :--- | :--- | :--- |
| `STORAGE_API_KEY` | The Bearer token required for authentication. No TLS, so take care. | `test-secret-key` |
| `STORAGE_DIR` | Local directory for file storage. | `/data` |
| `PORT` | Listening port for the HTTP server. | `8080` |
| `MAX_BUCKET_SIZE_GB` | Maximum allowed total size per bucket. | `1` |

## Getting Started

### Using Docker Compose

```yaml
services:
  storage-service:
    image: ghcr.io/dgramaciotti/simple-storage:latest
    ports:
      - "8080:8080"
    environment:
      - STORAGE_API_KEY=your-secure-key
      - MAX_BUCKET_SIZE_GB=1
    volumes:
      - ./storage-data:/data
```

### Direct Go Run

```bash
export STORAGE_API_KEY=my-key
go run main.go
```

## 📡 API Usage

All requests must include the `Authorization: Bearer <key>` header.

### Upload a File
`PUT /{bucket}/{key}`
```bash
curl -X PUT -H "Authorization: Bearer my-key" \
     -H "Content-Type: image/png" \
     --data-binary @image.png \
     http://localhost:8080/my-bucket/logo.png
```

### Download a File
`GET /{bucket}/{key}`
```bash
curl -H "Authorization: Bearer my-key" \
     http://localhost:8080/my-bucket/logo.png > downloaded.png
```

### List Buckets and Files
`GET /list`
```bash
curl -H "Authorization: Bearer my-key" http://localhost:8080/list
```

**Response Example:**
```json
{
  "buckets": [
    {
      "name": "my-bucket",
      "files": [
        {
          "key": "logo.png",
          "size": 168095,
          "content_type": "image/png",
          "modified_at": "2026-04-08T17:00:00Z"
        }
      ]
    }
  ]
}
```

## Security Architecture

This service is designed with **Defense in Depth**:

1.  **Isolation**: Should be deployed in a private network, accessible only by your backend.
2.  **Jailing**: All paths are cleaned and validated to prevent escaping the storage root.
3.  **Persistence**: Metadata (Content-Type) is stored in `.meta` sidecar files to ensure original MIME types are preserved regardless of naming.

## License

This project is licensed under the **MIT License** - see the [LICENSE](LICENSE) file for details.
