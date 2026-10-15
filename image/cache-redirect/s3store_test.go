package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// fakeS3 records the calls the store makes, and keeps what it would
// commit.
type fakeS3 struct {
	mu        sync.Mutex
	objects   map[string][]byte
	parts     map[int32][]byte
	calls     []string
	failPart  int32 // UploadPart of this number fails
	completed bool
	aborted   bool
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}, parts: map[int32][]byte{}} }

func (f *fakeS3) record(c string) { f.mu.Lock(); f.calls = append(f.calls, c); f.mu.Unlock() }

func (f *fakeS3) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return nil, errors.New("unused")
}

func (f *fakeS3) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return nil, errors.New("unused")
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.record("PutObject")
	b, _ := io.ReadAll(in.Body)
	f.mu.Lock()
	f.objects[*in.Key] = b
	f.mu.Unlock()
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	f.record("CreateMultipartUpload")
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("u1")}, nil
}

func (f *fakeS3) UploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	f.record("UploadPart")
	if *in.PartNumber == f.failPart {
		return nil, errors.New("part failed")
	}
	b, _ := io.ReadAll(in.Body)
	f.mu.Lock()
	f.parts[*in.PartNumber] = b
	f.mu.Unlock()
	return &s3.UploadPartOutput{ETag: aws.String(fmt.Sprintf("e%d", *in.PartNumber))}, nil
}

func (f *fakeS3) CompleteMultipartUpload(
	_ context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options),
) (*s3.CompleteMultipartUploadOutput, error) {
	f.record("CompleteMultipartUpload")
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []byte
	for i, p := range in.MultipartUpload.Parts {
		if *p.PartNumber != int32(i+1) || *p.ETag != fmt.Sprintf("e%d", i+1) {
			return nil, fmt.Errorf("parts out of order: %v", in.MultipartUpload.Parts)
		}
		all = append(all, f.parts[*p.PartNumber]...)
	}
	f.objects[*in.Key] = all
	f.completed = true
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (f *fakeS3) AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.record("AbortMultipartUpload")
	f.mu.Lock()
	f.aborted = true
	f.mu.Unlock()
	return &s3.AbortMultipartUploadOutput{}, nil
}

const testPart = 5 << 20

// failingReader yields n bytes, then err.
type failingReader struct {
	r   io.Reader
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, f.err
	}
	return n, err
}

func TestS3PutSmallIsOnePut(t *testing.T) {
	api := newFakeS3()
	s := newS3StoreWith(api, nil, "b", testPart, 2)
	n, err := s.Put(context.Background(), "k", strings.NewReader("small"), "text/plain")
	if err != nil || n != 5 {
		t.Fatalf("Put = %d, %v", n, err)
	}
	if string(api.objects["k"]) != "small" || strings.Join(api.calls, ",") != "PutObject" {
		t.Errorf("calls %v, object %q", api.calls, api.objects["k"])
	}
}

func TestS3PutLargeIsMultipart(t *testing.T) {
	api := newFakeS3()
	s := newS3StoreWith(api, nil, "b", testPart, 2)
	body := bytes.Repeat([]byte("0123456789abcdef"), (12<<20)/16) // 12 MiB: three parts
	n, err := s.Put(context.Background(), "k", bytes.NewReader(body), "application/zip")
	if err != nil || n != int64(len(body)) {
		t.Fatalf("Put = %d, %v", n, err)
	}
	if !api.completed || api.aborted || !bytes.Equal(api.objects["k"], body) || len(api.parts) != 3 {
		t.Errorf("completed %v aborted %v parts %d, object intact %v", api.completed, api.aborted, len(api.parts), bytes.Equal(api.objects["k"], body))
	}
}

// The property the whole service rests on: a reader that ends in anything
// but io.EOF never becomes an object, whether it fails inside the first
// part or several parts in.
func TestS3PutNeverCommitsAFailedReader(t *testing.T) {
	truncated := fmt.Errorf("upstream body ended early: %w", io.ErrUnexpectedEOF)
	for name, size := range map[string]int{"within one part": 1000, "exactly one part": testPart, "several parts": 11 << 20} {
		api := newFakeS3()
		s := newS3StoreWith(api, nil, "b", testPart, 2)
		r := &failingReader{r: bytes.NewReader(make([]byte, size)), err: truncated}
		if _, err := s.Put(context.Background(), "k", r, "x"); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s: Put = %v, want the reader's error", name, err)
		}
		if _, ok := api.objects["k"]; ok || api.completed {
			t.Errorf("%s: a failed reader was committed (calls %v)", name, api.calls)
		}
		if size >= testPart && !api.aborted {
			t.Errorf("%s: the multipart upload was not aborted (calls %v)", name, api.calls)
		}
	}
}

func TestS3PutAbortsOnAFailedPart(t *testing.T) {
	api := newFakeS3()
	api.failPart = 2
	s := newS3StoreWith(api, nil, "b", testPart, 1)
	if _, err := s.Put(context.Background(), "k", bytes.NewReader(make([]byte, 16<<20)), "x"); err == nil {
		t.Fatal("Put succeeded with a failed part")
	}
	if api.completed || !api.aborted {
		t.Errorf("completed %v aborted %v", api.completed, api.aborted)
	}
}

func TestMapS3Err(t *testing.T) {
	status := func(code int) error {
		return &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: code}}, Err: errors.New("x"),
		}}
	}
	if !errors.Is(mapS3Err(&types.NoSuchKey{}), ErrNotFound) || !errors.Is(mapS3Err(&types.NotFound{}), ErrNotFound) ||
		!errors.Is(mapS3Err(status(404)), ErrNotFound) {
		t.Error("a missing key is not ErrNotFound")
	}
	if errors.Is(mapS3Err(status(403)), ErrNotFound) {
		t.Error("a 403 was taken for a miss")
	}
}
