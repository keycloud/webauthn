// packed implements the Packed (WebAuthn spec section 8.2) attestation statement format
package packed

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"math/big"

	"github.com/keycloud/webauthn/protocol"
)

func init() {
	protocol.RegisterFormat("packed", verifyPacked)
}

var extensionIDFIDOGenCAAAGUID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 45724, 1, 1, 4}

func verifyPacked(a protocol.Attestation, clientDataHash []byte) error {
	rawAlg, ok := a.AttStmt["alg"]
	if !ok {
		return protocol.ErrInvalidAttestation.WithDebug("missing alg for packed")
	}
	algInt, ok := rawAlg.(int64)
	if !ok {
		return protocol.ErrInvalidAttestation.WithDebugf("invalid alg for packed, is of invalid type %T", rawAlg)
	}

	alg := protocol.COSEAlgorithmIdentifier(algInt)

	rawSig, ok := a.AttStmt["sig"]
	if !ok {
		return protocol.ErrInvalidAttestation.WithDebug("missing sig for packed")
	}
	sig, ok := rawSig.([]byte)
	if !ok {
		return protocol.ErrInvalidAttestation.WithDebug("invalid sig for packed")
	}

	// 2. If x5c is present, this indicates that the attestation type is not ECDAA. In this case:
	if _, ok := a.AttStmt["x5c"]; ok {
		return verifyBasic(a, clientDataHash, alg, sig)
	}

	// 3. If ecdaaKeyId is present, then the attestation type is ECDAA. In this case:
	if _, ok := a.AttStmt["ecdaaKeyId"]; ok {
		return verifyECDAA(a, clientDataHash, alg, sig)
	}

	// 4. If neither x5c nor ecdaaKeyId is present, self attestation is in use.
	return verifySelf(a, clientDataHash, alg, sig)
}

func verifyBasic(a protocol.Attestation, clientDataHash []byte, alg protocol.COSEAlgorithmIdentifier, sig []byte) error {
	x5c, ok := a.AttStmt["x5c"].([]interface{})
	if !ok {
		return protocol.ErrInvalidAttestation.WithDebug("invalid x5c for packed")
	}

	// let attCert be that element
	attestnCert, ok := x5c[0].([]byte)
	if !ok {
		return protocol.ErrInvalidAttestation.WithDebug("invalid x5c for packed")
	}

	// Let certificate public key be the public key conveyed by attCert
	cert, err := x509.ParseCertificate(attestnCert)
	if err != nil {
		return protocol.ErrInvalidAttestation.WithDebugf("invalid x5c for packed: %v", err)
	}

	// 2.1 Verify that sig is a valid signature over the concatenation of authenticatorData and clientDataHash using
	// the attestation public key in attestnCert with the algorithm specified in alg.
	signedBytes := append(a.AuthData.Raw, clientDataHash...)
	if err := cert.CheckSignature(cert.SignatureAlgorithm, signedBytes, sig); err != nil {
		// Fallback to ECDSAWithSA256 if signature algorithm is incorret, as is the case with Yubico's keys
		err = cert.CheckSignature(x509.ECDSAWithSHA256, signedBytes, sig)
		if err != nil {
			return protocol.ErrInvalidAttestation.WithDebugf("invalid signature for packed: %v", err)
		}
	}

	// 2.2 Verify that attestnCert meets the requirements in §8.2.1 Packed attestation statement certificate requirements.

	// Version MUST be set to 3 (which is indicated by an ASN.1 INTEGER with value 2).
	if cert.Version != 3 {
		return protocol.ErrInvalidAttestation.WithDebug("invalid version for certificate")
	}

	// The Basic Constraints extension MUST have the CA component set to false.
	if cert.IsCA {
		return protocol.ErrInvalidAttestation.WithDebug("CA is set for certificate")
	}

	var aaguidValue []byte

	for _, ext := range cert.Extensions {
		// If the related attestation root certificate is used for multiple authenticator models, the Extension
		// OID 1.3.6.1.4.1.45724.1.1.4 (id-fido-gen-ce-aaguid) MUST be present, containing the AAGUID as a 16-byte
		// OCTET STRING.
		if ext.Id.Equal(extensionIDFIDOGenCAAAGUID) {
			// The extension MUST NOT be marked as critical.
			if ext.Critical {
				return protocol.ErrInvalidAttestation.WithDebugf("extension id-fido-gen-ce-aaguid is present, but is marked as critical")
			}
			aaguidValue = ext.Value
		}
	}

	// 2.3 If attestnCert contains an extension with OID 1.3.6.1.4.1.45724.1.1.4 (id-fido-gen-ce-aaguid) verify that
	// the value of this extension matches the aaguid in authenticatorData.
	if len(aaguidValue) > 0 {
		// Note that an X.509 Extension encodes the DER-encoding of the value in an OCTET STRING. Thus, the AAGUID MUST
		// be wrapped in two OCTET STRINGS to be valid
		var aaguid []byte
		if _, err := asn1.Unmarshal(aaguidValue, &aaguid); err != nil {
			return protocol.ErrInvalidAttestation.WithDebugf("invalid AAGUID: %v", err)
		}

		if !bytes.Equal(a.AuthData.AttestedCredentialData.AAGUID, aaguid) {
			return protocol.ErrInvalidAttestation.WithDebugf("invalid AAGUID")
		}

	}

	// If successful, return attestation type Basic and attestation trust path x5c.
	return nil
}

func verifyECDAA(a protocol.Attestation, clientDataHash []byte, alg protocol.COSEAlgorithmIdentifier, sig []byte) error {
	return protocol.ErrInvalidAttestation.WithDebugf("unsupported packed format ECDAA")
}

func verifySelf(a protocol.Attestation, clientDataHash []byte, alg protocol.COSEAlgorithmIdentifier, sig []byte) error {
	// 4.1 Validate that alg matches the algorithm of the credentialPublicKey in authenticatorData.

	// 4.2 Verify that sig is a valid signature over the concatenation of authenticatorData and clientDataHash using
	// the credential public key with alg.
	signedBytes := append(a.AuthData.Raw, clientDataHash...)

	switch v := a.AuthData.AttestedCredentialData.COSEKey.(type) {
	case *ecdsa.PublicKey:
		// Right now, only EC256 is supported
		if alg != protocol.ES256 || v.Curve != elliptic.P256() {
			return protocol.ErrInvalidAttestation.WithDebugf("unsupported packed self attestation ECDSA key curve %s", v.Curve.Params().Name)
		}

		// 6.4.5.1 Signature Formats for Packed Attestation ES256
		signature := make([]*big.Int, 2)
		if rest, err := asn1.Unmarshal(sig, signature); err != nil {
			return protocol.ErrInvalidAttestation.WithDebugf("invalid ECDSA signature: %v", err).WithCause(err)
		} else if rest != nil {
			return protocol.ErrInvalidAttestation.WithDebugf("invalid ECDSA signature: too much data")
		}

		hash := sha256.Sum256(signedBytes)
		if !ecdsa.Verify(v, hash[:], signature[0], signature[1]) {
			return protocol.ErrInvalidAttestation.WithDebugf("invalid signature for packed")
		}
	default:
		return protocol.ErrInvalidAttestation.WithDebugf("unsupported packed self attestation public key type %T", a.AuthData.AttestedCredentialData.COSEKey)
	}

	// If successful, return implementation-specific values representing attestation type Self and an empty attestation trust path.
	return nil
}
