package s3

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type WriteAbortCloser interface {
	io.WriteCloser
	Abort() error
}

type Object struct {
	c   *S3
	Key string
}

// ObjectHead represents the headers returned by a HEAD request.
type ObjectHead struct {
	http.Header
}

func (oh *ObjectHead) Date() (time.Time, error) {
	return time.Parse(time.RFC1123, oh.Get("Date"))
}

func (oh *ObjectHead) LastModified() (time.Time, error) {
	return time.Parse(time.RFC1123, oh.Get("Last-Modified"))
}

func (oh *ObjectHead) ETag() string {
	return oh.Get("ETag")
}

func (oh *ObjectHead) ContentLength() (int64, error) {
	return strconv.ParseInt(oh.Get("Content-Length"), 10, 64)
}

func (oh *ObjectHead) ContentType() string {
	return oh.Get("Content-Type")
}

type ACL string

const (
	Private           = ACL("private")
	PublicRead        = ACL("public-read")
	PublicReadWrite   = ACL("public-read-write")
	AuthenticatedRead = ACL("authenticated-read")
	BucketOwnerRead   = ACL("bucket-owner-read")
	BucketOwnerFull   = ACL("bucket-owner-full-control")
)

// FormUpload returns a new signed form upload url
func (o *Object) FormUploadURL(acl ACL, policy Policy, customParams ...url.Values) (*url.URL, error) {
	b, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}

	policy64 := base64.StdEncoding.EncodeToString(b)
	mac := hmac.New(sha1.New, []byte(o.c.Secret))
	mac.Write([]byte(policy64))

	u := o.c.url("")
	val := make(url.Values)
	val.Set("AWSAccessKeyId", o.c.Key)
	val.Set("acl", string(acl))
	val.Set("key", o.Key)
	val.Set("signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	val.Set("policy", policy64)
	for _, p := range customParams {
		for k, v := range p {
			for _, v2 := range v {
				val.Add(k, v2)
			}
		}
	}

	u.RawQuery = val.Encode()

	return u, nil
}

// AuthenticatedURL produces a signed URL that can be used to access private resources
func (o *Object) AuthenticatedURL(useHttps bool, method string, expiresIn time.Duration) (*url.URL, error) {
	// Create signature string
	//
	// Make sure to always use + instead of %20, otherwise
	// we might get problems when pre-authorizing requests because
	// spaces are escaped differently in the path and query.
	key := strings.Replace(o.urlSafeKey(), `+`, `%20`, -1)
	expires := strconv.FormatInt(time.Now().Add(expiresIn).Unix(), 10)
	toSign := method + "\n\n\n" + expires + "\n/" + o.c.Bucket + `/` + key

	// Generate signature
	mac := hmac.New(sha1.New, []byte(o.c.Secret))
	mac.Write([]byte(toSign))

	sig := strings.TrimSpace(base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	// Assemble url
	var v = make(url.Values)
	v.Set("AWSAccessKeyId", o.c.Key)
	v.Set("Expires", expires)
	v.Set("Signature", sig)

	scheme := "http"
	if useHttps {
		scheme = "https"
	}
	u, err := url.Parse(scheme + "://s3.amazonaws.com")
	if err != nil {
		return nil, err
	}
	u.Path = `/` + o.c.Bucket + `/` + o.Key
	u.RawQuery = v.Encode()

	return u, nil
}

// Delete deletes the S3 object.
func (o *Object) Delete() error {
	_, err := o.request("DELETE", 204)
	return err
}

// Exists tests if an object already exists.
func (o *Object) Exists() (bool, error) {
	resp, err := o.request("HEAD", 0)
	if err != nil {
		return false, err
	}
	return (resp.StatusCode == 200), nil
}

// Head gets the objects meta information.
func (o *Object) Head() (*ObjectHead, error) {
	resp, err := o.request("HEAD", 0)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 200 {
		return &ObjectHead{resp.Header}, nil
	}
	return nil, errors.New(http.StatusText(resp.StatusCode))
}

// Writer returns a new WriteAbortCloser you can write to.
// The written data will be uploaded as a multipart request.
func (o *Object) Writer() (WriteAbortCloser, error) {
	return newUploader(o.c, o.urlSafeKey())
}

// Reader returns a new ReadCloser you can read from.
func (o *Object) Reader() (io.ReadCloser, http.Header, error) {
	resp, err := o.request("GET", 200)
	if err != nil {
		return nil, nil, err
	}
	return resp.Body, resp.Header, nil
}

func (o *Object) urlSafeKey() string {
	comp := strings.Split(o.Key, `/`)
	a := make([]string, 0, len(comp))
	for _, s := range comp {
		a = append(a, url.QueryEscape(s))
	}
	return strings.Join(a, `/`)
}

func (o *Object) request(method string, expectCode int) (*http.Response, error) {
	req, err := http.NewRequest(method, o.c.url(o.urlSafeKey()).String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	o.c.signRequest(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if expectCode != 0 && resp.StatusCode != expectCode {
		return nil, newS3Error(resp)
	}
	return resp, nil
}
