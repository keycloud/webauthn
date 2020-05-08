// attestation can be imported to import all supported attestation formats
package attestation

import (
	_ "github.com/keycloud/webauthn/attestation/androidsafetynet"
	_ "github.com/keycloud/webauthn/attestation/fido"
	_ "github.com/keycloud/webauthn/attestation/packed"
)
