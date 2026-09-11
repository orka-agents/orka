// Package service exposes the clean-room Workspace/Publisher boundary around
// internal/publisher.
//
// Every stateful endpoint requires both an exact controller bearer token and a
// short-lived HMAC operation capability bound to method, path, canonical request
// digest, namespace, Task or Publication identity, and operation ID. The API
// accepts only credential file references; it never accepts raw SCM credentials.
// Workspace source preparation creates a fresh exact-ref bare clone, reads only
// validated Git blobs, preserves validated relative symlinks while rejecting unsafe links, submodules, and Git metadata, writes a
// deterministic content-addressed tar, and uploads it through the controller
// artifact API. Publication endpoints delegate deterministic prepare, exact-ref
// CAS publish, and independent verification to internal/publisher.
package service
