package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// maxParts is S3's limit on the parts of one multipart upload.
const maxParts = 10000

// s3API is the part of the S3 client the store calls, so a test can stand
// in for it.
type s3API interface {
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

type presignAPI interface {
	PresignGetObject(context.Context, *s3.GetObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// s3Store is the Store over one S3 (or S3-compatible) bucket.
//
// An object smaller than one part is a single PutObject of a buffered
// body, sent only once the reader has ended cleanly. Anything larger is a
// multipart upload, completed only once the reader has ended cleanly and
// aborted otherwise: S3 makes a multipart object visible at
// CompleteMultipartUpload and never before, which is exactly the "never a
// truncated object" property the cache needs. A bucket lifecycle rule
// (AbortIncompleteMultipartUpload) is the backstop for an abort that
// itself fails, e.g. a pod killed mid-upload.
type s3Store struct {
	api         s3API
	presign     presignAPI
	bucket      string
	partSize    int
	concurrency int
	bufs        sync.Pool
}

// s3Options are the store's settings, from flags.
type s3Options struct {
	Bucket, Region, Endpoint string
	PathStyle                bool
	PartSize, Concurrency    int
	PresignTTL               time.Duration
}

// newS3Store builds the store from the default AWS credential chain: EKS
// Pod Identity (the container credential provider), IRSA, or the static
// keys an S3-compatible store needs in the environment.
func newS3Store(ctx context.Context, o s3Options) (*s3Store, error) {
	opts := []func(*config.LoadOptions) error{
		// A presigned URL is valid only as long as the credentials that
		// signed it. Refreshing them this long before they expire keeps
		// every URL handed out good for its whole TTL.
		config.WithCredentialsCacheOptions(func(c *aws.CredentialsCacheOptions) {
			c.ExpiryWindow = o.PresignTTL + time.Minute
		}),
	}
	if o.Region != "" {
		opts = append(opts, config.WithRegion(o.Region))
	}
	if o.Endpoint != "" {
		// S3-compatible stores do not all accept the checksum headers the
		// SDK adds by default; send them only where the API requires.
		opts = append(opts,
			config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
			config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("no S3 region: set -s3-region (or AWS_REGION)")
	}
	client := s3.NewFromConfig(cfg, func(so *s3.Options) {
		if o.Endpoint != "" {
			so.BaseEndpoint = aws.String(o.Endpoint)
		}
		so.UsePathStyle = o.PathStyle
	})
	return newS3StoreWith(client, s3.NewPresignClient(client), o.Bucket, o.PartSize, o.Concurrency), nil
}

func newS3StoreWith(api s3API, presign presignAPI, bucket string, partSize, concurrency int) *s3Store {
	s := &s3Store{api: api, presign: presign, bucket: bucket, partSize: partSize, concurrency: max(concurrency, 1)}
	s.bufs.New = func() any { b := make([]byte, s.partSize); return &b }
	return s
}

func (s *s3Store) getBuf() *[]byte  { return s.bufs.Get().(*[]byte) }
func (s *s3Store) putBuf(b *[]byte) { s.bufs.Put(b) }

func (s *s3Store) Head(ctx context.Context, key string) (int64, error) {
	out, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return 0, mapS3Err(err)
	}
	return aws.ToInt64(out.ContentLength), nil
}

func (s *s3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, mapS3Err(err)
	}
	return out.Body, nil
}

func (s *s3Store) Presign(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

func (s *s3Store) Put(ctx context.Context, key string, r io.Reader, contentType string) (int64, error) {
	buf := s.getBuf()
	n, err := readFull(r, *buf)
	switch {
	case errors.Is(err, io.EOF) && n < len(*buf):
		// The whole object fits one part, and the reader ended cleanly.
		defer s.putBuf(buf)
		_, perr := s.api.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &s.bucket, Key: &key, ContentType: &contentType,
			Body: bytes.NewReader((*buf)[:n]), ContentLength: aws.Int64(int64(n)),
		})
		if perr != nil {
			return 0, perr
		}
		return int64(n), nil
	case err != nil && !errors.Is(err, io.EOF):
		s.putBuf(buf)
		return 0, err
	}
	return s.multipart(ctx, key, r, contentType, buf, n, err)
}

// multipart uploads first and the rest of r as parts, up to s.concurrency
// at a time; the reader is held back while that many are in flight, so
// memory stays at (concurrency+1) parts per upload.
func (s *s3Store) multipart(ctx context.Context, key string, r io.Reader, contentType string, first *[]byte, n int, rerr error) (int64, error) {
	up, err := s.api.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &s.bucket, Key: &key, ContentType: &contentType})
	if err != nil {
		s.putBuf(first)
		return 0, err
	}
	uctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		parts []types.CompletedPart
		upErr error
		total int64
	)
	sem := make(chan struct{}, s.concurrency)
	failed := func() error { mu.Lock(); defer mu.Unlock(); return upErr }
	send := func(num int32, b *[]byte, size int) {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer s.putBuf(b)
			out, err := s.api.UploadPart(uctx, &s3.UploadPartInput{
				Bucket: &s.bucket, Key: &key, UploadId: up.UploadId, PartNumber: aws.Int32(num),
				Body: bytes.NewReader((*b)[:size]), ContentLength: aws.Int64(int64(size)),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if upErr == nil {
					upErr = err
					cancel()
				}
				return
			}
			parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(num)})
		}()
	}

	buf := first
	for num := int32(1); ; num++ {
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			s.putBuf(buf)
			break
		}
		if n > 0 {
			total += int64(n)
			send(num, buf, n)
		} else {
			s.putBuf(buf)
		}
		if rerr != nil { // io.EOF: the reader ended cleanly
			rerr = nil
			break
		}
		if rerr = failed(); rerr != nil {
			break
		}
		if num == maxParts {
			rerr = fmt.Errorf("object exceeds %d parts of %d bytes", maxParts, s.partSize)
			break
		}
		buf = s.getBuf()
		n, rerr = readFull(r, *buf)
	}
	if rerr != nil {
		cancel()
	}
	wg.Wait()
	if rerr == nil {
		rerr = failed()
	}
	if rerr != nil {
		s.abort(ctx, key, up.UploadId)
		return 0, rerr
	}
	sort.Slice(parts, func(i, j int) bool { return aws.ToInt32(parts[i].PartNumber) < aws.ToInt32(parts[j].PartNumber) })
	_, err = s.api.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &s.bucket, Key: &key, UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		s.abort(ctx, key, up.UploadId)
		return 0, err
	}
	return total, nil
}

// abort discards an upload on a context of its own: the one the upload
// ran on may be the reason it is being aborted.
func (s *s3Store) abort(ctx context.Context, key string, id *string) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, _ = s.api.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{Bucket: &s.bucket, Key: &key, UploadId: id})
}

// readFull fills buf from r. Unlike io.ReadFull it returns r's own error
// unchanged -- io.EOF only when r ended cleanly -- because the reader's
// io.ErrUnexpectedEOF means "the upstream body was truncated", and that
// must never be mistaken for a short last part.
func readFull(r io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// mapS3Err turns S3's "no such key" (NoSuchKey on GET, a bare 404 on
// HEAD) into ErrNotFound and leaves every other error, 403 included, as
// it is.
func mapS3Err(err error) error {
	var nf *types.NotFound
	var nsk *types.NoSuchKey
	if errors.As(err, &nf) || errors.As(err, &nsk) {
		return ErrNotFound
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == 404 {
		return ErrNotFound
	}
	return err
}
