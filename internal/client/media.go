package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/astelmach20/gwsgo/internal/discovery"
)

const (
	// chunkGranularity is mandated by Google: every chunk except the last must
	// be a multiple of 256 KiB.
	chunkGranularity = 256 * 1024

	// defaultChunkSize is the upload chunk size (8 MiB, a multiple of the
	// granularity). Larger chunks are faster but lose more work on a retry.
	defaultChunkSize = 32 * chunkGranularity

	// multipartCeiling is the documented limit for simple/multipart uploads.
	// Anything larger must go through the resumable protocol.
	multipartCeiling = 5 * 1024 * 1024

	// exportCeiling is the documented cap on files.export output.
	exportCeiling = 10 * 1024 * 1024
)

// uploadBase rewrites a method's base URL onto the media upload host path.
func uploadBase(doc *discovery.Document, protocolPath string) string {
	return strings.TrimRight(doc.RootURL, "/") + "/" + strings.TrimLeft(protocolPath, "/")
}

// detectContentType guesses a MIME type from the file extension.
func detectContentType(path, override string) string {
	if override != "" {
		return override
	}
	if guessed := mime.TypeByExtension(filepath.Ext(path)); guessed != "" {
		return guessed
	}
	return "application/octet-stream"
}

// Upload sends local file content for a method that supports media upload,
// choosing multipart for small files and the resumable protocol for large ones.
func (c *Client) Upload(ctx context.Context, req Request, path, contentType string) (any, error) {
	if req.Method.MediaUpload == nil {
		return nil, fmt.Errorf("%s does not accept file uploads", req.Method.ID)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("could not read upload file: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	mediaType := detectContentType(path, contentType)
	protocols := req.Method.MediaUpload.Protocols

	if info.Size() <= multipartCeiling && protocols.Simple != nil {
		return c.uploadMultipart(ctx, req, path, mediaType, protocols.Simple.Path)
	}
	if protocols.Resumable == nil {
		return nil, fmt.Errorf("%s is %d bytes, above the %d byte multipart limit, and %s "+
			"does not support resumable uploads", path, info.Size(), multipartCeiling, req.Method.ID)
	}
	return c.uploadResumable(ctx, req, path, mediaType, info.Size(), protocols.Resumable.Path)
}

// uploadMultipart sends metadata and content in a single related part body.
func (c *Client) uploadMultipart(ctx context.Context, req Request, path, mediaType, protocolPath string) (any, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	metadata := req.Body
	if metadata == nil {
		metadata = map[string]any{}
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	metaHeader := textproto.MIMEHeader{}
	metaHeader.Set("Content-Type", "application/json; charset=UTF-8")
	metaPart, err := writer.CreatePart(metaHeader)
	if err != nil {
		return nil, err
	}
	if _, err := metaPart.Write(encodedMetadata); err != nil {
		return nil, err
	}
	mediaHeader := textproto.MIMEHeader{}
	mediaHeader.Set("Content-Type", mediaType)
	mediaPart, err := writer.CreatePart(mediaHeader)
	if err != nil {
		return nil, err
	}
	if _, err := mediaPart.Write(content); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	params := copyParams(req.Params)
	params["uploadType"] = "multipart"
	endpoint, err := buildUploadURL(req.Doc, req.Method, params, protocolPath)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{
		"Content-Type": "multipart/related; boundary=" + writer.Boundary(),
	}
	resp, err := c.send(ctx, req.Method.HTTPMethod, endpoint, buffer.Bytes(), headers)
	if err != nil {
		return nil, err
	}
	return decode(resp)
}

// uploadResumable performs the chunked resumable upload protocol.
//
// Initiate with X-Upload-Content-*, read the session URI from Location, then
// PUT chunks with Content-Range. A 308 means "resume incomplete" and the Range
// response header reports the last byte the server actually stored, which is
// not necessarily the end of the chunk we just sent.
func (c *Client) uploadResumable(ctx context.Context, req Request, path, mediaType string, size int64, protocolPath string) (any, error) {
	params := copyParams(req.Params)
	params["uploadType"] = "resumable"
	endpoint, err := buildUploadURL(req.Doc, req.Method, params, protocolPath)
	if err != nil {
		return nil, err
	}

	metadata := req.Body
	if metadata == nil {
		metadata = map[string]any{}
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	initHeaders := map[string]string{
		"Content-Type":            "application/json; charset=UTF-8",
		"X-Upload-Content-Type":   mediaType,
		"X-Upload-Content-Length": strconv.FormatInt(size, 10),
	}
	resp, err := c.send(ctx, req.Method.HTTPMethod, endpoint, encodedMetadata, initHeaders)
	if err != nil {
		return nil, err
	}
	if resp.status < 200 || resp.status >= 300 {
		return decode(resp)
	}
	session := resp.header.Get("Location")
	if session == "" {
		return nil, fmt.Errorf("resumable upload was not given a session URI")
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	token, err := c.Tokens.AccessToken(ctx)
	if err != nil {
		return nil, err
	}

	buffer := make([]byte, defaultChunkSize)
	var offset int64
	for offset < size {
		read, err := file.ReadAt(buffer, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if read == 0 {
			break
		}
		payload := buffer[:read]
		last := offset + int64(read) - 1

		chunkReq, err := http.NewRequestWithContext(ctx, http.MethodPut, session, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		chunkReq.Header.Set("Authorization", "Bearer "+token)
		chunkReq.Header.Set("Content-Type", mediaType)
		chunkReq.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, last, size))
		chunkReq.ContentLength = int64(read)

		chunkResp, err := c.HTTP.Do(chunkReq)
		if err != nil {
			return nil, err
		}
		chunkBody, readErr := io.ReadAll(chunkResp.Body)
		if closeErr := chunkResp.Body.Close(); closeErr != nil && readErr == nil {
			readErr = closeErr
		}
		if readErr != nil {
			return nil, readErr
		}
		chunk := &response{status: chunkResp.StatusCode, header: chunkResp.Header.Clone(), body: chunkBody}

		switch chunk.status {
		case 308:
			next, err := resumeOffset(chunk.header.Get("Range"))
			if err != nil {
				return nil, err
			}
			if next <= offset {
				return nil, fmt.Errorf("resumable upload made no progress at offset %d", offset)
			}
			offset = next
		default:
			return decode(chunk)
		}
	}
	return nil, fmt.Errorf("resumable upload ended without a final response")
}

// resumeOffset parses a "bytes=0-42" Range header into the next byte to send.
func resumeOffset(header string) (int64, error) {
	if header == "" {
		// No Range header means the server holds nothing yet.
		return 0, nil
	}
	_, span, found := strings.Cut(header, "=")
	if !found {
		return 0, fmt.Errorf("malformed Range header %q", header)
	}
	_, end, found := strings.Cut(span, "-")
	if !found {
		return 0, fmt.Errorf("malformed Range header %q", header)
	}
	last, err := strconv.ParseInt(strings.TrimSpace(end), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed Range header %q: %w", header, err)
	}
	return last + 1, nil
}

// buildUploadURL points a method at its media upload path.
func buildUploadURL(doc *discovery.Document, method *discovery.Method, params map[string]any, protocolPath string) (string, error) {
	shadow := *method
	shadow.Path = strings.TrimPrefix(protocolPath, "/")
	uploadDoc := *doc
	uploadDoc.BaseURL = uploadBase(doc, "")
	uploadDoc.ServicePath = ""
	return BuildURL(&uploadDoc, &shadow, params)
}

// copyParams clones a parameter map so callers are never mutated.
func copyParams(params map[string]any) map[string]any {
	clone := make(map[string]any, len(params)+1)
	for name, value := range params {
		clone[name] = value
	}
	return clone
}

// Download streams a media response to a file, returning the bytes written.
//
// Google-native files (Docs, Sheets, Slides) cannot be fetched with alt=media;
// they must be exported to a concrete MIME type, and that export is capped at
// 10 MB server-side. We surface both facts rather than emitting a confusing
// fileNotDownloadable error.
func (c *Client) Download(ctx context.Context, req Request, destination string) (int64, error) {
	params := copyParams(req.Params)
	if _, explicit := params["alt"]; !explicit && !strings.HasSuffix(req.Method.ID, ".export") {
		params["alt"] = "media"
	}
	endpoint, err := BuildURL(req.Doc, req.Method, params)
	if err != nil {
		return 0, err
	}
	resp, err := c.send(ctx, http.MethodGet, endpoint, nil, nil)
	if err != nil {
		return 0, err
	}
	if resp.status < 200 || resp.status >= 300 {
		_, decodeErr := decode(resp)
		return 0, annotateDownloadError(decodeErr)
	}
	if err := os.WriteFile(destination, resp.body, 0o600); err != nil {
		return 0, err
	}
	if strings.HasSuffix(req.Method.ID, ".export") && int64(len(resp.body)) >= exportCeiling {
		fmt.Fprintf(os.Stderr,
			"warning: export hit the %d byte server-side cap; use files.download for larger files\n",
			exportCeiling)
	}
	return int64(len(resp.body)), nil
}

// annotateDownloadError explains Drive's two most confusing download failures.
func annotateDownloadError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "fileNotDownloadable"):
		return fmt.Errorf("%w\n\nThis is a Google-native file. Use `files export` with a target "+
			"mimeType (for example text/markdown or application/pdf) instead of a plain download", err)
	case strings.Contains(message, "fileNotExportable"):
		return fmt.Errorf("%w\n\nThis file type cannot be exported. Use `files download`, which "+
			"returns a long-running operation, instead", err)
	default:
		return err
	}
}
