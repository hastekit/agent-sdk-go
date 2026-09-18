package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
)

// S3API is the subset of the AWS SDK v2 S3 client used by S3Store.
type S3API interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type S3StoreConfig struct {
	// MountPath advertises the container directory populated by the host.
	MountPath    string
	Bucket       string
	Prefix       string // optional object key prefix
	MaxFileBytes int64  // zero: 20 MiB; uploads are buffered up to this limit
}

// S3Store stores private attachments as individual objects with their metadata.
// The caller owns the client and bucket. Bucket versioning is not required.
// Namespace access is determined by the caller, as with FileStore.
type S3Store struct {
	client                   S3API
	bucket, prefix, identity string
	maxBytes                 int64
	mountPath                string
}

func NewS3Store(client S3API, cfg S3StoreConfig) (*S3Store, error) {
	if err := validateMountPath(cfg.MountPath); err != nil {
		return nil, err
	}
	if client == nil || strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("%w: S3 client and bucket are required", ErrInvalid)
	}
	if cfg.MaxFileBytes < 0 || cfg.MaxFileBytes == math.MaxInt64 {
		return nil, fmt.Errorf("%w: invalid maximum file size", ErrInvalid)
	}
	if cfg.MaxFileBytes == 0 {
		cfg.MaxFileBytes = 20 << 20
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3Store{client: client, bucket: cfg.Bucket, prefix: prefix, identity: "s3:" + uuid.NewString(), maxBytes: cfg.MaxFileBytes, mountPath: cfg.MountPath}, nil
}

func (s *S3Store) namespace(ctx context.Context, namespace, sessionID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if namespace == ".metadata" || !validScopePart(namespace) || !validScopePart(sessionID) {
		return "", ErrDenied
	}
	return namespace + "/" + sessionID, nil
}

func (s *S3Store) Put(ctx context.Context, namespace, sessionID string, upload Upload) (Ref, error) {
	ns, err := s.namespace(ctx, namespace, sessionID)
	if err != nil {
		return Ref{}, err
	}
	media, _, err := mime.ParseMediaType(upload.MediaType)
	if err != nil || upload.Content == nil {
		return Ref{}, fmt.Errorf("%w: upload requires content and media type", ErrInvalid)
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, upload.Content}, s.maxBytes+1))
	if err != nil {
		return Ref{}, err
	}
	if int64(len(data)) > s.maxBytes {
		return Ref{}, ErrTooLarge
	}
	if len(data) == 0 {
		return Ref{}, fmt.Errorf("%w: empty file", ErrInvalid)
	}
	if InlineImageMediaType(media) && http.DetectContentType(data) != media {
		return Ref{}, fmt.Errorf("%w: image media type does not match content", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	hash := sha256.Sum256(data)
	version := hex.EncodeToString(hash[:])
	base := attachmentFilename(upload.Filename)
	uuidID := uuid.NewString()
	for number := 1; ; number++ {
		if err := ctx.Err(); err != nil {
			return Ref{}, err
		}
		id := numberedFilename(base, number)
		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + ns + "/" + id),
			Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String(media),
			IfNoneMatch: aws.String("*"),
			Metadata:    map[string]string{"sha256": version, "filename": base64.RawURLEncoding.EncodeToString([]byte(originalFilename(upload.Filename)))},
		})
		if err == nil {
			_, indexErr := s.client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + ".metadata/" + namespace + "/" + uuidID),
				Body: strings.NewReader("\n"), ContentLength: aws.Int64(1), IfNoneMatch: aws.String("*"),
				Metadata: map[string]string{"session": base64.RawURLEncoding.EncodeToString([]byte(sessionID)), "stored-filename": base64.RawURLEncoding.EncodeToString([]byte(id))},
			})
			if indexErr != nil {
				return Ref{}, s3StoreError(indexErr)
			}
			return Ref{ID: uuidID, SessionID: sessionID, Version: version}, nil
		}
		var api smithy.APIError
		if errors.As(err, &api) && (api.ErrorCode() == "PreconditionFailed" || api.ErrorCode() == "ConditionalRequestConflict") {
			continue
		}
		return Ref{}, s3StoreError(err)
	}

}

func (s *S3Store) Lookup(ctx context.Context, namespace, sessionID string, ref Ref) (Descriptor, error) {
	_, err := s.namespace(ctx, namespace, sessionID)
	if err != nil {
		return Descriptor{}, err
	}
	if ref.SessionID != "" && ref.SessionID != sessionID {
		return Descriptor{}, ErrDenied
	}
	if !validID(ref.ID) {
		return Descriptor{}, ErrInvalid
	}
	return s.lookupReference(ctx, namespace, sessionID, ref)
}

func (s *S3Store) LookupReference(ctx context.Context, namespace string, ref Ref) (Descriptor, error) {
	return s.lookupReference(ctx, namespace, "", ref)
}

func (s *S3Store) lookupReference(ctx context.Context, namespace, sessionID string, ref Ref) (Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return Descriptor{}, err
	}
	if !validScopePart(namespace) || namespace == ".metadata" {
		return Descriptor{}, ErrDenied
	}
	if !validID(ref.ID) {
		return Descriptor{}, ErrInvalid
	}
	index, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + ".metadata/" + namespace + "/" + ref.ID)})
	if err != nil {
		return Descriptor{}, s3StoreError(err)
	}
	storedSession, sessionErr := base64.RawURLEncoding.DecodeString(index.Metadata["session"])
	filename, filenameErr := base64.RawURLEncoding.DecodeString(index.Metadata["stored-filename"])
	if sessionErr != nil || filenameErr != nil || !validScopePart(string(storedSession)) || !validFilename(string(filename)) {
		return Descriptor{}, ErrInvalid
	}
	if (sessionID != "" && sessionID != string(storedSession)) || (ref.SessionID != "" && ref.SessionID != string(storedSession)) {
		return Descriptor{}, ErrDenied
	}
	ns := namespace + "/" + string(storedSession)
	key := s.prefix + ns + "/" + string(filename)
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return Descriptor{}, s3StoreError(err)
	}
	hash := out.Metadata["sha256"]
	rawHash, hashErr := hex.DecodeString(hash)
	original, nameErr := base64.RawURLEncoding.DecodeString(out.Metadata["filename"])
	media, _, mediaErr := mime.ParseMediaType(aws.ToString(out.ContentType))
	if hashErr != nil || len(rawHash) != sha256.Size || nameErr != nil || mediaErr != nil || aws.ToInt64(out.ContentLength) <= 0 || aws.ToString(out.ETag) == "" {
		return Descriptor{}, fmt.Errorf("%w: invalid S3 attachment metadata", ErrInvalid)
	}
	if ref.Version != "" && ref.Version != hash {
		return Descriptor{}, ErrNotFound
	}
	return Descriptor{Namespace: s.identity + ":" + ns, Key: key, Version: aws.ToString(out.ETag), SHA256: hash, Filename: string(original), StoredFilename: string(filename), MountPath: mountedPath(s.mountPath, string(filename)), MediaType: media, Size: aws.ToInt64(out.ContentLength)}, nil
}

// Open uses the ETag from Lookup to reject an object changed between metadata
// lookup and download. Attachment versions remain SHA-256 digests, not S3 ETags.
func (s *S3Store) Open(ctx context.Context, d Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ns, ok := strings.CutPrefix(d.Namespace, s.identity+":")
	parts := strings.Split(ns, "/")
	prefix := s.prefix + ns + "/"
	if !ok || len(parts) != 2 || !validScopePart(parts[0]) || !validScopePart(parts[1]) || !strings.HasPrefix(d.Key, prefix) || !validFilename(strings.TrimPrefix(d.Key, prefix)) || d.Version == "" {
		return nil, ErrDenied
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(d.Key), IfMatch: aws.String(d.Version)})
	if err != nil {
		return nil, s3StoreError(err)
	}
	if out.Body == nil {
		return nil, fmt.Errorf("%w: missing S3 object body", ErrInvalid)
	}
	return &contextReadCloser{contextReader{ctx, out.Body}, out.Body}, nil
}

func s3StoreError(err error) error {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchVersion", "PreconditionFailed":
			return fmt.Errorf("%w: %w", ErrNotFound, err)
		case "AccessDenied", "Forbidden":
			return fmt.Errorf("%w: %w", ErrDenied, err)
		}
	}
	return err
}

var _ UploadStore = (*S3Store)(nil)
var _ S3API = (*s3.Client)(nil)
