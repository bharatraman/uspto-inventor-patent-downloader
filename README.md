# USPTO Inventor Patent Downloader

A Go proof of concept that searches USPTO Open Data Portal records by
inventor and creates an inventor-specific package of complete US patent
documents.

## Current capabilities

- Searches every inventor position, rather than only the first inventor
- Retrieves all search-result pages
- Downloads one complete PDF for every distinct US patent grant
- Downloads a published application only when no grant exists
- Removes publications superseded by grants
- Processes requests sequentially
- Handles HTTP 429 responses with backoff
- Validates downloaded PDF files
- Reuses valid files on subsequent runs
- Writes a JSON manifest
- Creates an inventor-specific ZIP package

## Requirements

- Go
- USPTO Open Data Portal API key
- `github.com/patent-dev/uspto-odp`

## Configuration

Set the USPTO API key in the environment:

```bash
export USPTO_API_KEY="your-api-key"
