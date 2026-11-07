package enforcementprovider

func cloneBytes(v []byte) []byte {
	if v == nil {
		return nil
	}
	return append([]byte(nil), v...)
}
func cloneStrings(v []string) []string {
	if v == nil {
		return nil
	}
	return append([]string(nil), v...)
}
func cloneDescriptor(v Descriptor) Descriptor {
	v.ProtocolVersions = cloneStrings(v.ProtocolVersions)
	v.Capabilities = cloneStrings(v.Capabilities)
	v.PublicKey = cloneBytes(v.PublicKey)
	v.Signature = cloneBytes(v.Signature)
	return v
}
func cloneContext(v Context) Context {
	v.PolicyBytes = cloneBytes(v.PolicyBytes)
	v.RequiredCapabilities = cloneStrings(v.RequiredCapabilities)
	return v
}
func cloneAuthorization(v Authorization) Authorization { v.Context = cloneContext(v.Context); return v }
func cloneProof(v Proof) Proof                         { v.Signature = cloneBytes(v.Signature); return v }
func cloneAttempt(v Attempt) Attempt {
	v.Context = cloneContext(v.Context)
	v.Authorization = cloneAuthorization(v.Authorization)
	v.Descriptor = cloneDescriptor(v.Descriptor)
	v.Proof = cloneProof(v.Proof)
	return v
}
