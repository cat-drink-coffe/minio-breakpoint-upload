package minio_ext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/pkg/s3signer"
	"github.com/minio/minio-go/pkg/s3utils"
	"github.com/minio/minio-go/v6/pkg/credentials"
	"golang.org/x/net/publicsuffix"
)

// Global constants.
const (
	libraryName    = "minio-go"
	libraryVersion = "v6.0.44"
)

// User Agent should always following the below style.
// Please open an issue to discuss any new changes here.
//
//       MinIO (OS; ARCH) LIB/VER APP/VER
const (
	libraryUserAgentPrefix = "MinIO (" + runtime.GOOS + "; " + runtime.GOARCH + ") "
	libraryUserAgent       = libraryUserAgentPrefix + libraryName + "/" + libraryVersion
)

// requestMetadata - is container for all the values to make a request.
type requestMetadata struct {
	// If set newRequest presigns the URL.
	presignURL bool

	// User supplied.
	bucketName   string
	objectName   string
	queryValues  url.Values
	customHeader http.Header
	expires      int64

	// Generated by our internal code.
	bucketLocation   string
	contentBody      io.Reader
	contentLength    int64
	contentMD5Base64 string // carries base64 encoded md5sum
	contentSHA256Hex string // carries hex encoded sha256sum
}
type BucketLookupType int

// bucketLocationCache - Provides simple mechanism to hold bucket
// locations in memory.
type bucketLocationCache struct {
	// mutex is used for handling the concurrent
	// read/write requests for cache.
	sync.RWMutex

	// items holds the cached bucket locations.
	items map[string]string
}

// Client implements Amazon S3 compatible methods.
type Client struct {
	///  Standard options.

	// Parsed endpoint url provided by the user.
	endpointURL *url.URL

	// Holds various credential providers.
	credsProvider *credentials.Credentials

	// Custom signerType value overrides all credentials.
	overrideSignerType credentials.SignatureType

	// User supplied.
	appInfo struct {
		appName    string
		appVersion string
	}

	// Indicate whether we are using https or not
	secure bool

	// Needs allocation.
	httpClient     *http.Client
	bucketLocCache *bucketLocationCache

	// Advanced functionality.
	isTraceEnabled  bool
	traceErrorsOnly bool
	traceOutput     io.Writer

	// S3 specific accelerated endpoint.
	s3AccelerateEndpoint string

	// Region endpoint
	region string

	// Random seed.
	random *rand.Rand

	// lookup indicates type of url lookup supported by server. If not specified,
	// default to Auto.
	lookup BucketLookupType
}

// lockedRandSource provides protected rand source, implements rand.Source interface.
type lockedRandSource struct {
	lk  sync.Mutex
	src rand.Source
}

// Int63 returns a non-negative pseudo-random 63-bit integer as an int64.
func (r *lockedRandSource) Int63() (n int64) {
	r.lk.Lock()
	n = r.src.Int63()
	r.lk.Unlock()
	return
}

// Seed uses the provided seed value to initialize the generator to a
// deterministic state.
func (r *lockedRandSource) Seed(seed int64) {
	r.lk.Lock()
	r.src.Seed(seed)
	r.lk.Unlock()
}


// Different types of url lookup supported by the server.Initialized to BucketLookupAuto
const (
	BucketLookupAuto BucketLookupType = iota
	BucketLookupDNS
	BucketLookupPath
)

// awsS3EndpointMap Amazon S3 endpoint map.
var awsS3EndpointMap = map[string]string{
	"us-east-1":      "s3.dualstack.us-east-1.amazonaws.com",
	"us-east-2":      "s3.dualstack.us-east-2.amazonaws.com",
	"us-west-2":      "s3.dualstack.us-west-2.amazonaws.com",
	"us-west-1":      "s3.dualstack.us-west-1.amazonaws.com",
	"ca-central-1":   "s3.dualstack.ca-central-1.amazonaws.com",
	"eu-west-1":      "s3.dualstack.eu-west-1.amazonaws.com",
	"eu-west-2":      "s3.dualstack.eu-west-2.amazonaws.com",
	"eu-west-3":      "s3.dualstack.eu-west-3.amazonaws.com",
	"eu-central-1":   "s3.dualstack.eu-central-1.amazonaws.com",
	"eu-north-1":     "s3.dualstack.eu-north-1.amazonaws.com",
	"ap-east-1":      "s3.dualstack.ap-east-1.amazonaws.com",
	"ap-south-1":     "s3.dualstack.ap-south-1.amazonaws.com",
	"ap-southeast-1": "s3.dualstack.ap-southeast-1.amazonaws.com",
	"ap-southeast-2": "s3.dualstack.ap-southeast-2.amazonaws.com",
	"ap-northeast-1": "s3.dualstack.ap-northeast-1.amazonaws.com",
	"ap-northeast-2": "s3.dualstack.ap-northeast-2.amazonaws.com",
	"sa-east-1":      "s3.dualstack.sa-east-1.amazonaws.com",
	"us-gov-west-1":  "s3.dualstack.us-gov-west-1.amazonaws.com",
	"us-gov-east-1":  "s3.dualstack.us-gov-east-1.amazonaws.com",
	"cn-north-1":     "s3.cn-north-1.amazonaws.com.cn",
	"cn-northwest-1": "s3.cn-northwest-1.amazonaws.com.cn",
}

// Non exhaustive list of AWS S3 standard error responses -
// http://docs.aws.amazon.com/AmazonS3/latest/API/ErrorResponses.html
var s3ErrorResponseMap = map[string]string{
	"AccessDenied":                      "Access Denied.",
	"BadDigest":                         "The Content-Md5 you specified did not match what we received.",
	"EntityTooSmall":                    "Your proposed upload is smaller than the minimum allowed object size.",
	"EntityTooLarge":                    "Your proposed upload exceeds the maximum allowed object size.",
	"IncompleteBody":                    "You did not provide the number of bytes specified by the Content-Length HTTP header.",
	"InternalError":                     "We encountered an internal error, please try again.",
	"InvalidAccessKeyId":                "The access key ID you provided does not exist in our records.",
	"InvalidBucketName":                 "The specified bucket is not valid.",
	"InvalidDigest":                     "The Content-Md5 you specified is not valid.",
	"InvalidRange":                      "The requested range is not satisfiable",
	"MalformedXML":                      "The XML you provided was not well-formed or did not validate against our published schema.",
	"MissingContentLength":              "You must provide the Content-Length HTTP header.",
	"MissingContentMD5":                 "Missing required header for this request: Content-Md5.",
	"MissingRequestBodyError":           "Request body is empty.",
	"NoSuchBucket":                      "The specified bucket does not exist.",
	"NoSuchBucketPolicy":                "The bucket policy does not exist",
	"NoSuchKey":                         "The specified key does not exist.",
	"NoSuchUpload":                      "The specified multipart upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.",
	"NotImplemented":                    "A header you provided implies functionality that is not implemented",
	"PreconditionFailed":                "At least one of the pre-conditions you specified did not hold",
	"RequestTimeTooSkewed":              "The difference between the request time and the server's time is too large.",
	"SignatureDoesNotMatch":             "The request signature we calculated does not match the signature you provided. Check your key and signing method.",
	"MethodNotAllowed":                  "The specified method is not allowed against this resource.",
	"InvalidPart":                       "One or more of the specified parts could not be found.",
	"InvalidPartOrder":                  "The list of parts was not in ascending order. The parts list must be specified in order by part number.",
	"InvalidObjectState":                "The operation is not valid for the current state of the object.",
	"AuthorizationHeaderMalformed":      "The authorization header is malformed; the region is wrong.",
	"MalformedPOSTRequest":              "The body of your POST request is not well-formed multipart/form-data.",
	"BucketNotEmpty":                    "The bucket you tried to delete is not empty",
	"AllAccessDisabled":                 "All access to this bucket has been disabled.",
	"MalformedPolicy":                   "Policy has invalid resource.",
	"MissingFields":                     "Missing fields in request.",
	"AuthorizationQueryParametersError": "Error parsing the X-Amz-Credential parameter; the Credential is mal-formed; expecting \"<YOUR-AKID>/YYYYMMDD/REGION/SERVICE/aws4_request\".",
	"MalformedDate":                     "Invalid date format header, expected to be in ISO8601, RFC1123 or RFC1123Z time format.",
	"BucketAlreadyOwnedByYou":           "Your previous request to create the named bucket succeeded and you already own it.",
	"InvalidDuration":                   "Duration provided in the request is invalid.",
	"XAmzContentSHA256Mismatch":         "The provided 'x-amz-content-sha256' header does not match what was computed.",
	// Add new API errors here.
}

// List of success status.
var successStatus = []int{
	http.StatusOK,
	http.StatusNoContent,
	http.StatusPartialContent,
}

// newBucketLocationCache - Provides a new bucket location cache to be
// used internally with the client object.
func newBucketLocationCache() *bucketLocationCache {
	return &bucketLocationCache{
		items: make(map[string]string),
	}
}

// Redirect requests by re signing the request.
func (c *Client) redirectHeaders(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("stopped after 5 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	lastRequest := via[len(via)-1]
	var reAuth bool
	for attr, val := range lastRequest.Header {
		// if hosts do not match do not copy Authorization header
		if attr == "Authorization" && req.Host != lastRequest.Host {
			reAuth = true
			continue
		}
		if _, ok := req.Header[attr]; !ok {
			req.Header[attr] = val
		}
	}

	*c.endpointURL = *req.URL

	value, err := c.credsProvider.Get()
	if err != nil {
		return err
	}
	var (
		signerType      = value.SignerType
		accessKeyID     = value.AccessKeyID
		secretAccessKey = value.SecretAccessKey
		sessionToken    = value.SessionToken
		region          = c.region
	)

	// Custom signer set then override the behavior.
	if c.overrideSignerType != credentials.SignatureDefault {
		signerType = c.overrideSignerType
	}

	// If signerType returned by credentials helper is anonymous,
	// then do not sign regardless of signerType override.
	if value.SignerType == credentials.SignatureAnonymous {
		signerType = credentials.SignatureAnonymous
	}

	if reAuth {
		// Check if there is no region override, if not get it from the URL if possible.
		if region == "" {
			region = s3utils.GetRegionFromURL(*c.endpointURL)
		}
		switch {
		case signerType.IsV2():
			return errors.New("signature V2 cannot support redirection")
		case signerType.IsV4():
			s3signer.SignV4(*req, accessKeyID, secretAccessKey, sessionToken, getDefaultLocation(*c.endpointURL, region))
		}
	}
	return nil
}
func privateNew(endpoint string, creds *credentials.Credentials, secure bool, region string, lookup BucketLookupType) (*Client, error) {
	// construct endpoint.
	endpointURL, err := getEndpointURL(endpoint, secure)
	if err != nil {
		return nil, err
	}

	// Initialize cookies to preserve server sent cookies if any and replay
	// them upon each request.
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}

	// instantiate new Client.
	clnt := new(Client)

	// Save the credentials.
	clnt.credsProvider = creds

	// Remember whether we are using https or not
	clnt.secure = secure

	// Save endpoint URL, user agent for future uses.
	clnt.endpointURL = endpointURL

	transport, err := DefaultTransport(secure)
	if err != nil {
		return nil, err
	}

	// Instantiate http client and bucket location cache.
	clnt.httpClient = &http.Client{
		Jar:           jar,
		Transport:     transport,
		CheckRedirect: clnt.redirectHeaders,
	}

	// Sets custom region, if region is empty bucket location cache is used automatically.
	if region == "" {
		region = s3utils.GetRegionFromURL(*clnt.endpointURL)
	}
	clnt.region = region

	// Instantiate bucket location cache.
	clnt.bucketLocCache = newBucketLocationCache()

	// Introduce a new locked random seed.
	clnt.random = rand.New(&lockedRandSource{src: rand.NewSource(time.Now().UTC().UnixNano())})

	// Sets bucket lookup style, whether server accepts DNS or Path lookup. Default is Auto - determined
	// by the SDK. When Auto is specified, DNS lookup is used for Amazon/Google cloud endpoints and Path for all other endpoints.
	clnt.lookup = lookup
	// Return.
	return clnt, nil
}

// New - instantiate minio client, adds automatic verification of signature.
func New(endpoint, accessKeyID, secretAccessKey string, secure bool) (*Client, error) {
	creds := credentials.NewStaticV4(accessKeyID, secretAccessKey, "")
	clnt, err := privateNew(endpoint, creds, secure, "", BucketLookupAuto)
	if err != nil {
		return nil, err
	}
	// Google cloud storage should be set to signature V2, force it if not.
	if s3utils.IsGoogleEndpoint(*clnt.endpointURL) {
		clnt.overrideSignerType = credentials.SignatureV2
	}
	// If Amazon S3 set to signature v4.
	if s3utils.IsAmazonEndpoint(*clnt.endpointURL) {
		clnt.overrideSignerType = credentials.SignatureV4
	}
	return clnt, nil
}

// Get - Returns a value of a given key if it exists.
func (r *bucketLocationCache) Get(bucketName string) (location string, ok bool) {
	r.RLock()
	defer r.RUnlock()
	location, ok = r.items[bucketName]
	return
}

// set User agent.
func (c Client) setUserAgent(req *http.Request) {
	req.Header.Set("User-Agent", libraryUserAgent)
	if c.appInfo.appName != "" && c.appInfo.appVersion != "" {
		req.Header.Set("User-Agent", libraryUserAgent+" "+c.appInfo.appName+"/"+c.appInfo.appVersion)
	}
}

// getBucketLocationRequest - Wrapper creates a new getBucketLocation request.
func (c Client) getBucketLocationRequest(bucketName string) (*http.Request, error) {
	// Set location query.
	urlValues := make(url.Values)
	urlValues.Set("location", "")

	// Set get bucket location always as path style.
	targetURL := *c.endpointURL

	// as it works in makeTargetURL method from api.go file
	if h, p, err := net.SplitHostPort(targetURL.Host); err == nil {
		if targetURL.Scheme == "http" && p == "80" || targetURL.Scheme == "https" && p == "443" {
			targetURL.Host = h
		}
	}

	targetURL.Path = path.Join(bucketName, "") + "/"
	targetURL.RawQuery = urlValues.Encode()

	// Get a new HTTP request for the method.
	req, err := http.NewRequest("GET", targetURL.String(), nil)
	if err != nil {
		return nil, err
	}

	// Set UserAgent for the request.
	c.setUserAgent(req)

	// Get credentials from the configured credentials provider.
	value, err := c.credsProvider.Get()
	if err != nil {
		return nil, err
	}

	var (
		signerType      = value.SignerType
		accessKeyID     = value.AccessKeyID
		secretAccessKey = value.SecretAccessKey
		sessionToken    = value.SessionToken
	)

	// Custom signer set then override the behavior.
	if c.overrideSignerType != credentials.SignatureDefault {
		signerType = c.overrideSignerType
	}

	// If signerType returned by credentials helper is anonymous,
	// then do not sign regardless of signerType override.
	if value.SignerType == credentials.SignatureAnonymous {
		signerType = credentials.SignatureAnonymous
	}

	if signerType.IsAnonymous() {
		return req, nil
	}

	if signerType.IsV2() {
		// Get Bucket Location calls should be always path style
		isVirtualHost := false
		req = s3signer.SignV2(*req, accessKeyID, secretAccessKey, isVirtualHost)
		return req, nil
	}

	// Set sha256 sum for signature calculation only with signature version '4'.
	contentSha256 := emptySHA256Hex
	if c.secure {
		contentSha256 = unsignedPayload
	}

	req.Header.Set("X-Amz-Content-Sha256", contentSha256)
	req = s3signer.SignV4(*req, accessKeyID, secretAccessKey, sessionToken, "us-east-1")
	return req, nil
}

// dumpHTTP - dump HTTP request and response.
func (c Client) dumpHTTP(req *http.Request, resp *http.Response) error {
	// Starts http dump.
	_, err := fmt.Fprintln(c.traceOutput, "---------START-HTTP---------")
	if err != nil {
		return err
	}

	// Filter out Signature field from Authorization header.
	origAuth := req.Header.Get("Authorization")
	if origAuth != "" {
		req.Header.Set("Authorization", redactSignature(origAuth))
	}

	// Only display request header.
	reqTrace, err := httputil.DumpRequestOut(req, false)
	if err != nil {
		return err
	}

	// Write request to trace output.
	_, err = fmt.Fprint(c.traceOutput, string(reqTrace))
	if err != nil {
		return err
	}

	// Only display response header.
	var respTrace []byte

	// For errors we make sure to dump response body as well.
	if resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusPartialContent &&
		resp.StatusCode != http.StatusNoContent {
		respTrace, err = httputil.DumpResponse(resp, true)
		if err != nil {
			return err
		}
	} else {
		respTrace, err = httputil.DumpResponse(resp, false)
		if err != nil {
			return err
		}
	}

	// Write response to trace output.
	_, err = fmt.Fprint(c.traceOutput, strings.TrimSuffix(string(respTrace), "\r\n"))
	if err != nil {
		return err
	}

	// Ends the http dump.
	_, err = fmt.Fprintln(c.traceOutput, "---------END-HTTP---------")
	if err != nil {
		return err
	}

	// Returns success.
	return nil
}

// do - execute http request.
func (c Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Handle this specifically for now until future Golang versions fix this issue properly.
		if urlErr, ok := err.(*url.Error); ok {
			if strings.Contains(urlErr.Err.Error(), "EOF") {
				return nil, &url.Error{
					Op:  urlErr.Op,
					URL: urlErr.URL,
					Err: errors.New("Connection closed by foreign host " + urlErr.URL + ". Retry again."),
				}
			}
		}
		return nil, err
	}

	// Response cannot be non-nil, report error if thats the case.
	if resp == nil {
		msg := "Response is empty. " + reportIssue
		return nil, ErrInvalidArgument(msg)
	}

	// If trace is enabled, dump http request and response,
	// except when the traceErrorsOnly enabled and the response's status code is ok
	if c.isTraceEnabled && !(c.traceErrorsOnly && resp.StatusCode == http.StatusOK) {
		err = c.dumpHTTP(req, resp)
		if err != nil {
			return nil, err
		}
	}

	return resp, nil
}

// getBucketLocation - Get location for the bucketName from location map cache, if not
// fetch freshly by making a new request.
func (c Client) getBucketLocation(bucketName string) (string, error) {
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		return "", err
	}

	// Region set then no need to fetch bucket location.
	if c.region != "" {
		return c.region, nil
	}

	if location, ok := c.bucketLocCache.Get(bucketName); ok {
		return location, nil
	}

	// Initialize a new request.
	req, err := c.getBucketLocationRequest(bucketName)
	if err != nil {
		return "", err
	}

	// Initiate the request.
	resp, err := c.do(req)
	defer closeResponse(resp)
	if err != nil {
		return "", err
	}
	location, err := processBucketLocationResponse(resp, bucketName)
	if err != nil {
		return "", err
	}
	c.bucketLocCache.Set(bucketName, location)
	return location, nil
}

// Set - Will persist a value into cache.
func (r *bucketLocationCache) Set(bucketName string, location string) {
	r.Lock()
	defer r.Unlock()
	r.items[bucketName] = location
}

// processes the getBucketLocation http response from the server.
func processBucketLocationResponse(resp *http.Response, bucketName string) (bucketLocation string, err error) {
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			err = httpRespToErrorResponse(resp, bucketName, "")
			errResp := ToErrorResponse(err)
			// For access denied error, it could be an anonymous
			// request. Move forward and let the top level callers
			// succeed if possible based on their policy.
			switch errResp.Code {
			case "AuthorizationHeaderMalformed":
				fallthrough
			case "InvalidRegion":
				fallthrough
			case "AccessDenied":
				if errResp.Region == "" {
					return "us-east-1", nil
				}
				return errResp.Region, nil
			}
			return "", err
		}
	}

	// Extract location.
	var locationConstraint string
	err = xmlDecoder(resp.Body, &locationConstraint)
	if err != nil {
		return "", err
	}

	location := locationConstraint
	// Location is empty will be 'us-east-1'.
	if location == "" {
		location = "us-east-1"
	}

	// Location can be 'EU' convert it to meaningful 'eu-west-1'.
	if location == "EU" {
		location = "eu-west-1"
	}

	// Save the location into cache.

	// Return.
	return location, nil
}

// Get default location returns the location based on the input
// URL `u`, if region override is provided then all location
// defaults to regionOverride.
//
// If no other cases match then the location is set to `us-east-1`
// as a last resort.
func getDefaultLocation(u url.URL, regionOverride string) (location string) {
	if regionOverride != "" {
		return regionOverride
	}
	region := s3utils.GetRegionFromURL(u)
	if region == "" {
		region = "us-east-1"
	}
	return region
}

// returns true if virtual hosted style requests are to be used.
func (c *Client) isVirtualHostStyleRequest(url url.URL, bucketName string) bool {
	if bucketName == "" {
		return false
	}

	if c.lookup == BucketLookupDNS {
		return true
	}
	if c.lookup == BucketLookupPath {
		return false
	}

	// default to virtual only for Amazon/Google  storage. In all other cases use
	// path style requests
	return s3utils.IsVirtualHostSupported(url, bucketName)
}

// ErrTransferAccelerationBucket - bucket name is invalid to be used with transfer acceleration.
func ErrTransferAccelerationBucket(bucketName string) error {
	return ErrorResponse{
		StatusCode: http.StatusBadRequest,
		Code:       "InvalidArgument",
		Message:    "The name of the bucket used for Transfer Acceleration must be DNS-compliant and must not contain periods ‘.’.",
		BucketName: bucketName,
	}
}

// getS3Endpoint get Amazon S3 endpoint based on the bucket location.
func getS3Endpoint(bucketLocation string) (s3Endpoint string) {
	s3Endpoint, ok := awsS3EndpointMap[bucketLocation]
	if !ok {
		// Default to 's3.dualstack.us-east-1.amazonaws.com' endpoint.
		s3Endpoint = "s3.dualstack.us-east-1.amazonaws.com"
	}
	return s3Endpoint
}

// makeTargetURL make a new target url.
func (c Client) makeTargetURL(bucketName, objectName, bucketLocation string, isVirtualHostStyle bool, queryValues url.Values) (*url.URL, error) {
	host := c.endpointURL.Host
	// For Amazon S3 endpoint, try to fetch location based endpoint.
	if s3utils.IsAmazonEndpoint(*c.endpointURL) {
		if c.s3AccelerateEndpoint != "" && bucketName != "" {
			// http://docs.aws.amazon.com/AmazonS3/latest/dev/transfer-acceleration.html
			// Disable transfer acceleration for non-compliant bucket names.
			if strings.Contains(bucketName, ".") {
				return nil, ErrTransferAccelerationBucket(bucketName)
			}
			// If transfer acceleration is requested set new host.
			// For more details about enabling transfer acceleration read here.
			// http://docs.aws.amazon.com/AmazonS3/latest/dev/transfer-acceleration.html
			host = c.s3AccelerateEndpoint
		} else {
			// Do not change the host if the endpoint URL is a FIPS S3 endpoint.
			if !s3utils.IsAmazonFIPSEndpoint(*c.endpointURL) {
				// Fetch new host based on the bucket location.
				host = getS3Endpoint(bucketLocation)
			}
		}
	}

	// Save scheme.
	scheme := c.endpointURL.Scheme

	// Strip port 80 and 443 so we won't send these ports in Host header.
	// The reason is that browsers and curl automatically remove :80 and :443
	// with the generated presigned urls, then a signature mismatch error.
	if h, p, err := net.SplitHostPort(host); err == nil {
		if scheme == "http" && p == "80" || scheme == "https" && p == "443" {
			host = h
		}
	}

	urlStr := scheme + "://" + host + "/"
	// Make URL only if bucketName is available, otherwise use the
	// endpoint URL.
	if bucketName != "" {
		// If endpoint supports virtual host style use that always.
		// Currently only S3 and Google Cloud Storage would support
		// virtual host style.
		if isVirtualHostStyle {
			urlStr = scheme + "://" + bucketName + "." + host + "/"
			if objectName != "" {
				urlStr = urlStr + s3utils.EncodePath(objectName)
			}
		} else {
			// If not fall back to using path style.
			urlStr = urlStr + bucketName + "/"
			if objectName != "" {
				urlStr = urlStr + s3utils.EncodePath(objectName)
			}
		}
	}

	// If there are any query values, add them to the end.
	if len(queryValues) > 0 {
		urlStr = urlStr + "?" + s3utils.QueryEncode(queryValues)
	}

	return url.Parse(urlStr)
}

// newRequest - instantiate a new HTTP request for a given method.
func (c Client) newRequest(method string, metadata requestMetadata) (req *http.Request, err error) {
	// If no method is supplied default to 'POST'.
	if method == "" {
		method = "POST"
	}

	location := metadata.bucketLocation
	if location == "" {
		if metadata.bucketName != "" {
			// Gather location only if bucketName is present.
			location, err = c.getBucketLocation(metadata.bucketName)
			if err != nil {
				return nil, err
			}
		}
		if location == "" {
			location = getDefaultLocation(*c.endpointURL, c.region)
		}
	}

	// Look if target url supports virtual host.
	// We explicitly disallow MakeBucket calls to not use virtual DNS style,
	// since the resolution may fail.
	isMakeBucket := (metadata.objectName == "" && method == "PUT" && len(metadata.queryValues) == 0)
	isVirtualHost := c.isVirtualHostStyleRequest(*c.endpointURL, metadata.bucketName) && !isMakeBucket

	// Construct a new target URL.
	targetURL, err := c.makeTargetURL(metadata.bucketName, metadata.objectName, location,
		isVirtualHost, metadata.queryValues)
	if err != nil {
		return nil, err
	}

	// Initialize a new HTTP request for the method.
	req, err = http.NewRequest(method, targetURL.String(), nil)
	if err != nil {
		return nil, err
	}

	// Get credentials from the configured credentials provider.
	value, err := c.credsProvider.Get()
	if err != nil {
		return nil, err
	}

	var (
		signerType      = value.SignerType
		accessKeyID     = value.AccessKeyID
		secretAccessKey = value.SecretAccessKey
		sessionToken    = value.SessionToken
	)

	// Custom signer set then override the behavior.
	if c.overrideSignerType != credentials.SignatureDefault {
		signerType = c.overrideSignerType
	}

	// If signerType returned by credentials helper is anonymous,
	// then do not sign regardless of signerType override.
	if value.SignerType == credentials.SignatureAnonymous {
		signerType = credentials.SignatureAnonymous
	}

	// Generate presign url if needed, return right here.
	if metadata.expires != 0 && metadata.presignURL {
		if signerType.IsAnonymous() {
			return nil, ErrInvalidArgument("Presigned URLs cannot be generated with anonymous credentials.")
		}
		if signerType.IsV2() {
			// Presign URL with signature v2.
			req = s3signer.PreSignV2(*req, accessKeyID, secretAccessKey, metadata.expires, isVirtualHost)
		} else if signerType.IsV4() {
			// Presign URL with signature v4.
			req = s3signer.PreSignV4(*req, accessKeyID, secretAccessKey, sessionToken, location, metadata.expires)
		}
		return req, nil
	}

	// Set 'User-Agent' header for the request.
	c.setUserAgent(req)

	// Set all headers.
	for k, v := range metadata.customHeader {
		req.Header.Set(k, v[0])
	}

	// Go net/http notoriously closes the request body.
	// - The request Body, if non-nil, will be closed by the underlying Transport, even on errors.
	// This can cause underlying *os.File seekers to fail, avoid that
	// by making sure to wrap the closer as a nop.
	if metadata.contentLength == 0 {
		req.Body = nil
	} else {
		req.Body = ioutil.NopCloser(metadata.contentBody)
	}

	// Set incoming content-length.
	req.ContentLength = metadata.contentLength
	if req.ContentLength <= -1 {
		// For unknown content length, we upload using transfer-encoding: chunked.
		req.TransferEncoding = []string{"chunked"}
	}

	// set md5Sum for content protection.
	if len(metadata.contentMD5Base64) > 0 {
		req.Header.Set("Content-Md5", metadata.contentMD5Base64)
	}

	// For anonymous requests just return.
	if signerType.IsAnonymous() {
		return req, nil
	}

	switch {
	case signerType.IsV2():
		// Add signature version '2' authorization header.
		req = s3signer.SignV2(*req, accessKeyID, secretAccessKey, isVirtualHost)
	case metadata.objectName != "" && method == "PUT" && metadata.customHeader.Get("X-Amz-Copy-Source") == "" && !c.secure:
		// Streaming signature is used by default for a PUT object request. Additionally we also
		// look if the initialized client is secure, if yes then we don't need to perform
		// streaming signature.
		req = s3signer.StreamingSignV4(req, accessKeyID,
			secretAccessKey, sessionToken, location, metadata.contentLength, time.Now().UTC())
	default:
		// Set sha256 sum for signature calculation only with signature version '4'.
		shaHeader := unsignedPayload
		if metadata.contentSHA256Hex != "" {
			shaHeader = metadata.contentSHA256Hex
		}
		req.Header.Set("X-Amz-Content-Sha256", shaHeader)

		// Add signature version '4' authorization header.
		req = s3signer.SignV4(*req, accessKeyID, secretAccessKey, sessionToken, location)
	}

	// Return request.
	return req, nil
}


func (c Client) GenUploadPartSignedUrl(uploadID string, bucketName string, objectName string, partNumber int, size int64, expires time.Duration, bucketLocation string) (string, error){
	signedUrl := ""

	// Input validation.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		return signedUrl, err
	}
	if err := s3utils.CheckValidObjectName(objectName); err != nil {
		return signedUrl, err
	}
	if size > maxPartSize {
		return signedUrl, errors.New("size is illegal")
	}
	if size <= -1 {
		return signedUrl, errors.New("size is illegal")
	}
	if partNumber <= 0 {
		return signedUrl, errors.New("partNumber is illegal")
	}
	if uploadID == "" {
		return signedUrl, errors.New("uploadID is illegal")
	}

	// Get resources properly escaped and lined up before using them in http request.
	urlValues := make(url.Values)
	// Set part number.
	urlValues.Set("partNumber", strconv.Itoa(partNumber))
	// Set upload id.
	urlValues.Set("uploadId", uploadID)

	// Set encryption headers, if any.
	customHeader := make(http.Header)

	reqMetadata := requestMetadata{
		presignURL:		  true,
		bucketName:       bucketName,
		objectName:       objectName,
		queryValues:      urlValues,
		customHeader:     customHeader,
		//contentBody:      reader,
		contentLength:    size,
		//contentMD5Base64: md5Base64,
		//contentSHA256Hex: sha256Hex,
		expires:		  int64(expires/time.Second),
		bucketLocation:		bucketLocation,
	}

	req, err := c.newRequest("PUT", reqMetadata)
	if err != nil {
		log.Println("newRequest failed:", err.Error())
		return signedUrl, err
	}

	signedUrl = req.URL.String()
	return signedUrl,nil
}


// executeMethod - instantiates a given method, and retries the
// request upon any error up to maxRetries attempts in a binomially
// delayed manner using a standard back off algorithm.
func (c Client) executeMethod(ctx context.Context, method string, metadata requestMetadata) (res *http.Response, err error) {
	var isRetryable bool     // Indicates if request can be retried.
	var bodySeeker io.Seeker // Extracted seeker from io.Reader.
	var reqRetry = MaxRetry  // Indicates how many times we can retry the request

	if metadata.contentBody != nil {
		// Check if body is seekable then it is retryable.
		bodySeeker, isRetryable = metadata.contentBody.(io.Seeker)
		switch bodySeeker {
		case os.Stdin, os.Stdout, os.Stderr:
			isRetryable = false
		}
		// Retry only when reader is seekable
		if !isRetryable {
			reqRetry = 1
		}

		// Figure out if the body can be closed - if yes
		// we will definitely close it upon the function
		// return.
		bodyCloser, ok := metadata.contentBody.(io.Closer)
		if ok {
			defer bodyCloser.Close()
		}
	}

	// Create a done channel to control 'newRetryTimer' go routine.
	doneCh := make(chan struct{}, 1)

	// Indicate to our routine to exit cleanly upon return.
	defer close(doneCh)

	// Blank indentifier is kept here on purpose since 'range' without
	// blank identifiers is only supported since go1.4
	// https://golang.org/doc/go1.4#forrange.
	for range c.newRetryTimer(reqRetry, DefaultRetryUnit, DefaultRetryCap, MaxJitter, doneCh) {
		// Retry executes the following function body if request has an
		// error until maxRetries have been exhausted, retry attempts are
		// performed after waiting for a given period of time in a
		// binomial fashion.
		if isRetryable {
			// Seek back to beginning for each attempt.
			if _, err = bodySeeker.Seek(0, 0); err != nil {
				// If seek failed, no need to retry.
				return nil, err
			}
		}

		// Instantiate a new request.
		var req *http.Request
		req, err = c.newRequest(method, metadata)
		if err != nil {
			errResponse := ToErrorResponse(err)
			if isS3CodeRetryable(errResponse.Code) {
				continue // Retry.
			}
			return nil, err
		}

		// Add context to request
		req = req.WithContext(ctx)

		// Initiate the request.
		res, err = c.do(req)
		if err != nil {
			// For supported http requests errors verify.
			if isHTTPReqErrorRetryable(err) {
				continue // Retry.
			}
			// For other errors, return here no need to retry.
			return nil, err
		}

		// For any known successful http status, return quickly.
		for _, httpStatus := range successStatus {
			if httpStatus == res.StatusCode {
				return res, nil
			}
		}

		// Read the body to be saved later.
		errBodyBytes, err := ioutil.ReadAll(res.Body)
		// res.Body should be closed
		closeResponse(res)
		if err != nil {
			return nil, err
		}

		// Save the body.
		errBodySeeker := bytes.NewReader(errBodyBytes)
		res.Body = ioutil.NopCloser(errBodySeeker)

		// For errors verify if its retryable otherwise fail quickly.
		errResponse := ToErrorResponse(httpRespToErrorResponse(res, metadata.bucketName, metadata.objectName))

		// Save the body back again.
		errBodySeeker.Seek(0, 0) // Seek back to starting point.
		res.Body = ioutil.NopCloser(errBodySeeker)

		// Bucket region if set in error response and the error
		// code dictates invalid region, we can retry the request
		// with the new region.
		//
		// Additionally we should only retry if bucketLocation and custom
		// region is empty.
		if metadata.bucketLocation == "" && c.region == "" {
			if errResponse.Code == "AuthorizationHeaderMalformed" || errResponse.Code == "InvalidRegion" {
				if metadata.bucketName != "" && errResponse.Region != "" {
					// Gather Cached location only if bucketName is present.
					if _, cachedLocationError := c.bucketLocCache.Get(metadata.bucketName); cachedLocationError != false {
						c.bucketLocCache.Set(metadata.bucketName, errResponse.Region)
						continue // Retry.
					}
				}
			}
		}

		// Verify if error response code is retryable.
		if isS3CodeRetryable(errResponse.Code) {
			continue // Retry.
		}

		// Verify if http status code is retryable.
		if isHTTPStatusRetryable(res.StatusCode) {
			continue // Retry.
		}

		// For all other cases break out of the retry loop.
		break
	}
	return res, err
}
