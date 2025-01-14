package mailbox

import (
	"bytes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/keychain"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// dhFn is the name of the function to be used for the DH operations of
	// the noise protocol.
	dhFn = "secp256k1"

	// cipherFun is the name of the cipher function to be used for during
	// the noise protocol.
	cipherFn = "ChaChaPoly"

	// hashFn is the name of the hash function to be used for the noise
	// protocol.
	hashFn = "SHA256"

	// macSize is the length in bytes of the tags generated by poly1305.
	macSize = 16

	// lengthHeaderSize is the number of bytes used to prefix encode the
	// length of a message payload.
	lengthHeaderSize = 2

	// encHeaderSize is the number of bytes required to hold an encrypted
	// header and it's MAC.
	encHeaderSize = lengthHeaderSize + macSize

	// keyRotationInterval is the number of messages sent on a single
	// cipher stream before the keys are rotated forwards.
	keyRotationInterval = 1000

	// handshakeReadTimeout is a read timeout that will be enforced when
	// waiting for data payloads during the various acts of Brontide. If
	// the remote party fails to deliver the proper payload within this
	// time frame, then we'll fail the connection.
	handshakeReadTimeout = time.Second * 5

	// HandshakeVersion0 is the handshake version in which the auth data
	// is sent in a fixed sized payload in act 2.
	HandshakeVersion0 = byte(0)

	// HandshakeVersion1 is the handshake version in which the auth data
	// is sent in a dynamically sized, length-prefixed payload in act 2.
	HandshakeVersion1 = byte(1)

	// HandshakeVersion2 is the handshake version that indicates that the
	// KK pattern is to be used for all handshakes after the first
	// handshake.
	HandshakeVersion2 = byte(2)

	// MinHandshakeVersion is the minimum handshake version that is
	// currently supported.
	MinHandshakeVersion = HandshakeVersion0

	// MaxHandshakeVersion is the maximum handshake that we currently
	// support. Any messages that carry a version not between
	// MinHandshakeVersion and MaxHandshakeVersion will cause the handshake
	// to abort immediately.
	MaxHandshakeVersion = HandshakeVersion2

	// ActTwoPayloadSize is the size of the fixed sized payload that can be
	// sent from the responder to the Initiator in act two.
	ActTwoPayloadSize = 500
)

var (
	// ErrMaxMessageLengthExceeded is returned a message to be written to
	// the cipher session exceeds the maximum allowed message payload.
	ErrMaxMessageLengthExceeded = errors.New("the generated payload exceeds " +
		"the max allowed message length of (2^16)-1")

	// ErrMessageNotFlushed signals that the connection cannot accept a new
	// message because the prior message has not been fully flushed.
	ErrMessageNotFlushed = errors.New("prior message not flushed")

	// lightningNodeConnectPrologue is the noise prologue that is used to
	// initialize the brontide noise handshake.
	lightningNodeConnectPrologue = []byte("lightning-node-connect")

	// ephemeralGen is the default ephemeral key generator, used to derive a
	// unique ephemeral key for each brontide handshake.
	ephemeralGen = func() (*btcec.PrivateKey, error) {
		return btcec.NewPrivateKey(btcec.S256())
	}

	// N is the generator point we'll use for our PAKE protocol. It was
	// generated via a try-and-increment approach using the phrase
	// "Lightning Node Connect" with SHA2-256.
	nBytes, _ = hex.DecodeString(
		"0254a58cd0f31c008fd0bc9b2dd5ba586144933829f6da33ac4130b555fb5ea32c",
	)
	N, _ = btcec.ParsePubKey(nBytes, btcec.S256())
)

// cipherState encapsulates the state for the AEAD which will be used to
// encrypt+authenticate any payloads sent during the handshake, and messages
// sent once the handshake has completed. During the handshake phase, each party
// has a single cipherState object but during the transport phase each party has
// two cipherState objects: one for sending and one for receiving.
type cipherState struct {
	// nonce is the nonce passed into the chacha20-poly1305 instance for
	// encryption+decryption. The nonce is incremented after each successful
	// encryption/decryption.
	nonce uint64

	// secretKey is the shared symmetric key which will be used to
	// instantiate the cipher.
	secretKey [32]byte

	// salt is an additional secret which is used during key rotation to
	// generate new keys.
	salt [32]byte

	// cipher is an instance of the ChaCha20-Poly1305 AEAD construction
	// created using the secretKey above.
	cipher cipher.AEAD
}

// Encrypt returns a ciphertext which is the encryption of the plainText
// observing the passed associatedData within the AEAD construction.
func (c *cipherState) Encrypt(associatedData, cipherText,
	plainText []byte) []byte {

	defer func() {
		c.nonce++

		if c.nonce == keyRotationInterval {
			c.rotateKey()
		}
	}()

	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[4:], c.nonce)

	// TODO(roasbeef): should just return plaintext?

	return c.cipher.Seal(cipherText, nonce[:], plainText, associatedData)
}

// Decrypt attempts to decrypt the passed ciphertext observing the specified
// associatedData within the AEAD construction. In the case that the final MAC
// check fails, then a non-nil error will be returned.
func (c *cipherState) Decrypt(associatedData, plainText,
	cipherText []byte) ([]byte, error) {

	defer func() {
		c.nonce++

		if c.nonce == keyRotationInterval {
			c.rotateKey()
		}
	}()

	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[4:], c.nonce)

	return c.cipher.Open(plainText, nonce[:], cipherText, associatedData)
}

// InitializeKey initializes the secret key and AEAD cipher scheme based off of
// the passed key.
func (c *cipherState) InitializeKey(key [32]byte) {
	c.secretKey = key
	c.nonce = 0

	// Safe to ignore the error here as our key is properly sized
	// (32-bytes).
	c.cipher, _ = chacha20poly1305.New(c.secretKey[:])
}

// InitializeKeyWithSalt is identical to InitializeKey however it also sets the
// cipherState's salt field which is used for key rotation.
func (c *cipherState) InitializeKeyWithSalt(salt, key [32]byte) {
	c.salt = salt
	c.InitializeKey(key)
}

// rotateKey rotates the current encryption/decryption key for this cipherState
// instance. Key rotation is performed by ratcheting the current key forward
// using an HKDF invocation with the cipherState's salt as the salt, and the
// current key as the input.
func (c *cipherState) rotateKey() {
	var (
		info    []byte
		nextKey [32]byte
	)

	oldKey := c.secretKey
	h := hkdf.New(sha256.New, oldKey[:], c.salt[:], info)

	// hkdf(ck, k, zero)
	// |
	// | \
	// |  \
	// ck  k'
	_, _ = h.Read(c.salt[:])
	_, _ = h.Read(nextKey[:])

	c.InitializeKey(nextKey)
}

// symmetricState encapsulates a cipherState object and houses the ephemeral
// handshake digest state. This struct is used during the handshake to derive
// new shared secrets based off of the result of ECDH operations. Ultimately,
// the final key yielded by this struct is the result of an incremental
// Triple-DH operation.
type symmetricState struct {
	cipherState

	// chainingKey is used as the salt to the HKDF function to derive a new
	// chaining key as well as a new tempKey which is used for
	// encryption/decryption.
	chainingKey [32]byte

	// tempKey is the latter 32 bytes resulted from the latest HKDF
	// iteration. This key is used to encrypt/decrypt any handshake
	// messages or payloads sent until the next DH operation is executed.
	tempKey [32]byte

	// handshakeDigest is the cumulative hash digest of all handshake
	// messages sent from start to finish. This value is never transmitted
	// to the other side, but will be used as the AD when
	// encrypting/decrypting messages using our AEAD construction.
	handshakeDigest [32]byte
}

// mixKey is implements a basic HKDF-based key ratchet. This method is called
// with the result of each DH output generated during the handshake process.
// The first 32 bytes extract from the HKDF reader is the next chaining key,
// then latter 32 bytes become the temp secret key using within any future AEAD
// operations until another DH operation is performed.
func (s *symmetricState) mixKey(input []byte) {
	var info []byte

	secret := input
	salt := s.chainingKey
	h := hkdf.New(sha256.New, secret, salt[:], info)

	// hkdf(ck, input, zero)
	// |
	// | \
	// |  \
	// ck  k
	_, _ = h.Read(s.chainingKey[:])
	_, _ = h.Read(s.tempKey[:])

	// cipher.k = temp_key
	s.InitializeKey(s.tempKey)
}

// mixHash hashes the passed input data into the cumulative handshake digest.
// The running result of this value (h) is used as the associated data in all
// decryption/encryption operations.
func (s *symmetricState) mixHash(data []byte) {
	h := sha256.New()
	_, _ = h.Write(s.handshakeDigest[:])
	_, _ = h.Write(data)

	copy(s.handshakeDigest[:], h.Sum(nil))
}

// EncryptAndHash returns the authenticated encryption of the passed plaintext.
// When encrypting the handshake digest (h) is used as the associated data to
// the AEAD cipher.
func (s *symmetricState) EncryptAndHash(plaintext []byte) []byte {
	ciphertext := s.Encrypt(s.handshakeDigest[:], nil, plaintext)

	s.mixHash(ciphertext)

	return ciphertext
}

// DecryptAndHash returns the authenticated decryption of the passed
// ciphertext.  When encrypting the handshake digest (h) is used as the
// associated data to the AEAD cipher.
func (s *symmetricState) DecryptAndHash(ciphertext []byte) ([]byte, error) {
	plaintext, err := s.Decrypt(s.handshakeDigest[:], nil, ciphertext)
	if err != nil {
		return nil, err
	}

	s.mixHash(ciphertext)

	return plaintext, nil
}

// InitializeSymmetric initializes the symmetric state by setting the handshake
// digest (h) and the chaining key (ck) to protocol name.
func (s *symmetricState) InitializeSymmetric(protocolName []byte) {
	var empty [32]byte

	s.handshakeDigest = sha256.Sum256(protocolName)
	s.chainingKey = s.handshakeDigest
	s.InitializeKey(empty)
}

// handshakeState encapsulates the symmetricState and keeps track of all the
// public keys (static and ephemeral) for both sides during the handshake
// transcript. If the handshake completes successfully, then two instances of a
// cipherState are emitted: one to encrypt messages from initiator to
// responder, and the other for the opposite direction.
type handshakeState struct {
	symmetricState

	initiator bool

	localStatic    keychain.SingleKeyECDH
	localEphemeral keychain.SingleKeyECDH // nolint (false positive)

	remoteStatic    *btcec.PublicKey // nolint
	remoteEphemeral *btcec.PublicKey // nolint

	passphraseEntropy []byte
	payloadToSend     []byte
	receivedPayload   []byte

	pattern HandshakePattern

	// minVersion is the minimum handshake version that the Machine
	// supports.
	minVersion byte

	// maxVersion is the maximum handshake version that the Machine
	// supports.
	maxVersion byte

	// version is handshake version that the client and server have agreed
	// on.
	version byte

	ephemeralGen func() (*btcec.PrivateKey, error)
}

// newHandshakeState returns a new instance of the handshake state initialized
// with the prologue and protocol name.
func newHandshakeState(minVersion, maxVersion byte, pattern HandshakePattern,
	initiator bool, prologue []byte, localPub keychain.SingleKeyECDH,
	remoteStatic *btcec.PublicKey, passphraseEntropy []byte,
	authData []byte,
	ephemeralGen func() (*btcec.PrivateKey, error)) (handshakeState,
	error) {

	startVersion := maxVersion
	if initiator {
		startVersion = minVersion
	}

	h := handshakeState{
		minVersion:        minVersion,
		maxVersion:        maxVersion,
		version:           startVersion,
		symmetricState:    symmetricState{},
		initiator:         initiator,
		localStatic:       localPub,
		remoteStatic:      remoteStatic,
		pattern:           pattern,
		passphraseEntropy: passphraseEntropy,
		payloadToSend:     authData,
		ephemeralGen:      ephemeralGen,
	}

	protocolName := fmt.Sprintf(
		"Noise_%s_%s_%s_%s", pattern.Name, dhFn, cipherFn, hashFn,
	)

	// Set the current chaining key and handshake digest to the hash of the
	// protocol name, and additionally mix in the prologue. If either sides
	// disagree about the prologue or protocol name, then the handshake
	// will fail.
	h.InitializeSymmetric([]byte(protocolName))
	h.mixHash(prologue)

	// TODO(roasbeef): if did mixHash here w/ the passphraseEntropy, then the same
	// as using it as a PSK?

	// Call mixHash once for every pub key listed in the pre-messages from
	// the handshake pattern.
	for _, m := range pattern.PreMessages {
		if m.Initiator == h.initiator {
			h.mixHash(localPub.PubKey().SerializeCompressed())
		} else {
			if remoteStatic == nil {
				return handshakeState{},
					errors.New("a remote static key is " +
						"expected for the given " +
						"handshake pattern")
			}
			h.mixHash(remoteStatic.SerializeCompressed())
		}
	}

	return h, nil
}

// writeMsgPattern uses the given MessagePattern to construct the data stream
// to write to the passed writer. First the handshake version is written, then
// the MessagePattern Tokens are processed and then any payload is encrypted
// in a way defined by the handshake version.
func (h *handshakeState) writeMsgPattern(w io.Writer, mp MessagePattern) error {
	buff := new(bytes.Buffer)

	// Write handshake byte to the buffer.
	if _, err := buff.Write([]byte{h.version}); err != nil {
		return err
	}

	// Write Tokens.
	if err := h.writeTokens(mp.Tokens, buff); err != nil {
		return err
	}

	// Write payload data.
	switch h.version {
	case HandshakeVersion0:
		var payload []byte
		switch mp.ActNum {
		case act1, act3:
		case act2:
			// If we have an auth payload, then we'll write
			// out 2 bytes that denotes the true length of
			// the payload, followed by the payload itself.
			var payloadWriter bytes.Buffer
			payload = make([]byte, ActTwoPayloadSize)

			if h.payloadToSend != nil {
				var length [lengthHeaderSize]byte
				payLoadLen := len(h.payloadToSend)
				binary.BigEndian.PutUint16(
					length[:], uint16(payLoadLen),
				)

				_, err := payloadWriter.Write(length[:])
				if err != nil {
					return err
				}

				_, err = payloadWriter.Write(h.payloadToSend)
				if err != nil {
					return err
				}
			}

			copy(payload, payloadWriter.Bytes())

		default:
			return fmt.Errorf("unknown act number: %d", mp.ActNum)
		}

		_, err := buff.Write(h.EncryptAndHash(payload))
		if err != nil {
			return err
		}

	case HandshakeVersion1, HandshakeVersion2:
		var payload []byte
		switch mp.ActNum {
		case act1, act3:
		case act2:
			payload = h.payloadToSend

			// The total length of each message payload including
			// the MAC size payload exceed the largest number
			// encodable within a 32-bit unsigned integer.
			if len(payload) >= math.MaxInt32 {
				return ErrMaxMessageLengthExceeded
			}

			// The full length of the packet is only the packet
			// length, and does NOT include the MAC.
			fullLength := uint32(len(payload))

			var pktLen [4]byte
			binary.BigEndian.PutUint32(pktLen[:], fullLength)

			// First, generate the encrypted+MAC'd length prefix for
			// the packet.
			_, err := buff.Write(h.EncryptAndHash(pktLen[:]))
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("unknown act number: %d", mp.ActNum)
		}

		// Finally, generate the encrypted packet itself.
		_, err := buff.Write(h.EncryptAndHash(payload))
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown handshake version: %v", h.version)
	}

	_, err := w.Write(buff.Bytes())
	return err
}

// readMsgPattern uses the given MessagePattern to process the data received
// from the passed reader. First the handshake version is checked, then
// the MessagePattern Tokens are processed and then any extra cipher text is
// decrypted as defined by the handshake version.
func (h *handshakeState) readMsgPattern(r io.Reader, mp MessagePattern) error {
	// Read and check version.
	var versionScratch [1]byte
	if _, err := r.Read(versionScratch[:]); err != nil {
		return err
	}
	version := versionScratch[0]

	switch mp.ActNum {
	case act1, act2:
		// During act 1 and 2 is when the initiator and responder are
		// negotiating their handshake versions. If the handshake
		// version is unknown or no longer supported, then the handshake
		// fails immediately.
		if version < h.minVersion || version > h.maxVersion {
			return fmt.Errorf("received unexpected handshake "+
				"version: %v", version)
		}

		// The initiator will adapt to the version chosen by the
		// responder since in our use case, the initiator is always
		// updated first and so will be aware of a new version before
		// the responder is.
		if h.initiator {
			h.version = version
		}

	case act3:
		// When the responder receives act 3, it is expected to be the
		// same version that it used in its act 2 message.
		if version != h.version {
			return fmt.Errorf("received unexpected handshake "+
				"version: %v", version)
		}

	default:
		return fmt.Errorf("unknown act number: %d", mp.ActNum)
	}

	// Read Tokens.
	if err := h.readTokens(r, mp.Tokens); err != nil {
		return err
	}

	// Read the payload data.
	switch version {
	case HandshakeVersion0:
		payloadSize := 0
		if mp.ActNum == act2 {
			payloadSize = ActTwoPayloadSize
		}

		cipherText := make([]byte, payloadSize+macSize)
		_, err := r.Read(cipherText)
		if err != nil {
			return err
		}

		payload, err := h.DecryptAndHash(cipherText)
		if err != nil {
			return err
		}

		// If the payload is a non-zero length, then we'll assume it's
		// the auth data and attempt to fully decode it.
		if len(payload) == 0 {
			return nil
		}

		payloadLen := binary.BigEndian.Uint16(
			payload[:lengthHeaderSize],
		)
		authData := make([]byte, payloadLen)

		payloadReader := bytes.NewReader(payload[lengthHeaderSize:])
		if _, err := payloadReader.Read(authData); err != nil {
			return err
		}

		h.receivedPayload = authData

	case HandshakeVersion1, HandshakeVersion2:
		var payloadSize uint32
		switch mp.ActNum {
		case act1, act3:
			payloadSize = macSize
		case act2:
			header := [4 + macSize]byte{}
			_, err := r.Read(header[:])
			if err != nil {
				return err
			}

			// Attempt to decrypt+auth the packet length present in
			// the stream.
			pktLenBytes, err := h.DecryptAndHash(header[:])
			if err != nil {
				return err
			}

			// Compute the packet length that we will need to read
			// off the wire.
			payloadSize = macSize +
				binary.BigEndian.Uint32(pktLenBytes)

		default:
			return fmt.Errorf("unknown act number: %d", mp.ActNum)
		}

		cipherText := make([]byte, payloadSize)
		_, err := r.Read(cipherText)
		if err != nil {
			return err
		}

		// Finally, decrypt the message held in the buffer, and return a
		// new byte slice containing the plaintext.
		authData, err := h.DecryptAndHash(cipherText)
		if err != nil {
			return err
		}

		if mp.ActNum == 2 {
			h.receivedPayload = authData
		}

	default:
		return fmt.Errorf("unknown handshake version: %v", h.version)
	}

	return nil
}

// writeToken processes a series of Tokens and either writes a public key
// (unencrypted, encrypted or masked) to the writer or performs a ECDH
// operation.
func (h *handshakeState) writeTokens(tokens []Token, w io.Writer) error {
	for _, token := range tokens {
		switch token {
		case e:
			e, err := h.ephemeralGen()
			if err != nil {
				return err
			}

			h.localEphemeral = &keychain.PrivKeyECDH{
				PrivKey: e,
			}

			pubKeyBytes := e.PubKey().SerializeCompressed()

			h.mixHash(pubKeyBytes)
			if _, err := w.Write(pubKeyBytes); err != nil {
				return err
			}

		case me:
			e, err := h.ephemeralGen()
			if err != nil {
				return err
			}

			h.localEphemeral = &keychain.PrivKeyECDH{
				PrivKey: e,
			}

			// Mix in the _unmasked_ ephemeral into the transcript
			// hash, as this allows us to use the MAC check to
			// assert if the remote party knows the passphraseEntropy or not.
			h.mixHash(e.PubKey().SerializeCompressed())

			// Now that we have our ephemeral, we'll apply the
			// eke-SPAKE2 specific portion by masking the key with
			// our passphraseEntropy.
			me := ekeMask(h.localEphemeral.PubKey(), h.passphraseEntropy)
			_, err = w.Write(me.SerializeCompressed())
			if err != nil {
				return err
			}

		case s:
			ct := h.EncryptAndHash(
				h.localStatic.PubKey().SerializeCompressed(),
			)

			if _, err := w.Write(ct); err != nil {
				return err
			}

		case ee:
			ee, err := ecdh(h.remoteEphemeral, h.localEphemeral)
			if err != nil {
				return err
			}
			h.mixKey(ee)

		case ss:
			ss, err := ecdh(h.remoteStatic, h.localStatic)
			if err != nil {
				return err
			}
			h.mixKey(ss)

		case es:
			var (
				es  []byte
				err error
			)
			if h.initiator {
				es, err = ecdh(h.remoteStatic, h.localEphemeral)
				if err != nil {
					return err
				}
			} else {
				es, err = ecdh(h.remoteEphemeral, h.localStatic)
				if err != nil {
					return err
				}
			}
			h.mixKey(es)

		case se:
			var (
				se  []byte
				err error
			)
			if h.initiator {
				se, err = ecdh(h.remoteEphemeral, h.localStatic)
				if err != nil {
					return err
				}
			} else {
				se, err = ecdh(h.remoteStatic, h.localEphemeral)
				if err != nil {
					return err
				}
			}
			h.mixKey(se)

		default:
			return fmt.Errorf("unknown token: %s", token)
		}
	}

	return nil
}

// readTokens uses the given Tokens to process data from the reader.
func (h *handshakeState) readTokens(r io.Reader, tokens []Token) error {
	for _, token := range tokens {
		switch token {
		case e:
			var e [33]byte
			_, err := r.Read(e[:])
			if err != nil {
				return err
			}

			h.remoteEphemeral, err = btcec.ParsePubKey(
				e[:], btcec.S256(),
			)
			if err != nil {
				return err
			}

			h.mixHash(h.remoteEphemeral.SerializeCompressed())

		case me:
			var me [33]byte
			_, err := r.Read(me[:])
			if err != nil {
				return err
			}

			maskedEphemeral, err := btcec.ParsePubKey(
				me[:], btcec.S256(),
			)
			if err != nil {
				return err
			}

			// Turn the masked ephemeral into a normal point, and
			// store that as the remote ephemeral key.
			h.remoteEphemeral = ekeUnmask(
				maskedEphemeral, h.passphraseEntropy,
			)

			// We mix in this _unmasked_ point as otherwise we will
			// fail the MC check below if we didn't recover the
			// correct point.
			h.mixHash(h.remoteEphemeral.SerializeCompressed())

		case s:
			s := [33 + macSize]byte{}
			_, err := r.Read(s[:])
			if err != nil {
				return err
			}

			rs, err := h.DecryptAndHash(s[:])
			if err != nil {
				return err
			}

			h.remoteStatic, err = btcec.ParsePubKey(
				rs, btcec.S256(),
			)
			if err != nil {
				return err
			}

		case ee:
			ee, err := ecdh(h.remoteEphemeral, h.localEphemeral)
			if err != nil {
				return err
			}
			h.mixKey(ee)

		case ss:
			ss, err := ecdh(h.remoteStatic, h.localStatic)
			if err != nil {
				return err
			}
			h.mixKey(ss)

		case es:
			var (
				es  []byte
				err error
			)
			if h.initiator {
				es, err = ecdh(h.remoteStatic, h.localEphemeral)
				if err != nil {
					return err
				}
			} else {
				es, err = ecdh(h.remoteEphemeral, h.localStatic)
				if err != nil {
					return err
				}
			}
			h.mixKey(es)

		case se:
			var (
				se  []byte
				err error
			)
			if h.initiator {
				se, err = ecdh(h.remoteEphemeral, h.localStatic)
				if err != nil {
					return err
				}
			} else {
				se, err = ecdh(h.remoteStatic, h.localEphemeral)
				if err != nil {
					return err
				}
			}
			h.mixKey(se)

		default:
			return fmt.Errorf("unknown token: %s", token)
		}
	}

	return nil
}

// Machine is a state-machine which implements Brontide: an Authenticated-key
// Exchange in Three Acts. Brontide is derived from the Noise framework,
// specifically implementing the Noise_XX handshake with an eke modified, where
// the public-key masking operation used is SPAKE2. Once the initial 3-act
// handshake has completed all messages are encrypted with a chacha20 AEAD
// cipher. On the wire, all messages are prefixed with an
// authenticated+encrypted length field. Additionally, the encrypted+auth'd
// length prefix is used as the AD when encrypting+decryption messages. This
// construction provides confidentiality of packet length, avoids introducing a
// padding-oracle, and binds the encrypted packet length to the packet itself.
//
// The acts proceeds the following order (initiator on the left):
//  GenActOne()   ->
//                    RecvActOne()
//                <-  GenActTwo()
//  RecvActTwo()
//  GenActThree() ->
//                    RecvActThree()
//
// This exchange corresponds to the following Noise handshake:
//   -> me
//   <- e, ee, s, es
//   -> s, se
//
// In this context, me is the masked ephemeral point that's masked using an
// operation derived from the traditional SPAKE2 protocol: e + h(pw)*M, where M
// is a generator of the cyclic group, h is a key derivation function, and pw is
// the passphraseEntropy known to both sides.
//
// Note that there's also another operating mode based on Noise_IK which can be
// used after the initial pairing is complete and both sides have exchange
// long-term public keys.
type Machine struct {
	cfg *BrontideMachineConfig

	sendCipher cipherState
	recvCipher cipherState

	handshakeState

	// nextCipherHeader is a static buffer that we'll use to read in the
	// next ciphertext header from the wire. The header is a 2 byte length
	// (of the next ciphertext), followed by a 16 byte MAC.
	nextCipherHeader [encHeaderSize]byte

	// nextHeaderSend holds a reference to the remaining header bytes to
	// write out for a pending message. This allows us to tolerate timeout
	// errors that cause partial writes.
	nextHeaderSend []byte

	// nextHeaderBody holds a reference to the remaining body bytes to write
	// out for a pending message. This allows us to tolerate timeout errors
	// that cause partial writes.
	nextBodySend []byte
}

// BrontideMachineConfig holds the config necessary to construct a new Machine
// object.
type BrontideMachineConfig struct {
	ConnData            ConnectionData
	Initiator           bool
	HandshakePattern    HandshakePattern
	MinHandshakeVersion byte
	MaxHandshakeVersion byte
	EphemeralGen        func() (*btcec.PrivateKey, error)
}

// NewBrontideMachine creates a new instance of the brontide state-machine.
func NewBrontideMachine(cfg *BrontideMachineConfig) (*Machine, error) {
	var (
		passphraseEntropy []byte
		err               error
	)

	var (
		minVersion = cfg.MinHandshakeVersion
		maxVersion = cfg.MaxHandshakeVersion
	)
	if cfg.HandshakePattern.Name == KK {
		// If the handshake pattern is KK, then we require that the
		// minimum and maximum handshake versions are at least 2.
		if maxVersion < HandshakeVersion2 {
			return nil, fmt.Errorf("a maximum handshake version " +
				"of at least 2 is require if the KK pattern " +
				"is to be used")
		}

		if minVersion < HandshakeVersion2 {
			minVersion = HandshakeVersion2
		}
	}

	// Since the passphraseEntropy is only used in the XX handshake pattern, we only
	// need to do the computationally expensive stretching operation if the
	// XX pattern is being used.
	if cfg.HandshakePattern.Name == XX {
		// We stretch the passphrase here in order to partially thwart
		// brute force attempts, and also ensure we obtain a high
		// entropy blinding point.
		passphraseEntropy, err = stretchPassphrase(
			cfg.ConnData.PassphraseEntropy(),
		)
		if err != nil {
			return nil, err
		}
	}

	if cfg.EphemeralGen == nil {
		cfg.EphemeralGen = ephemeralGen
	}

	handshake, err := newHandshakeState(
		minVersion, maxVersion, cfg.HandshakePattern, cfg.Initiator,
		lightningNodeConnectPrologue, cfg.ConnData.LocalKey(),
		cfg.ConnData.RemoteKey(), passphraseEntropy,
		cfg.ConnData.AuthData(), cfg.EphemeralGen,
	)
	if err != nil {
		return nil, err
	}

	return &Machine{
		cfg:            cfg,
		handshakeState: handshake,
	}, nil
}

// DoHandshake iterates over the MessagePattern of the Machine and processes
// it accordingly. After the handshake is complete, `split` is called in order
// to initialise the send and receive cipher states of the Machine.
func (b *Machine) DoHandshake(rw io.ReadWriter) error {
	for i := 0; i < len(b.pattern.Pattern); i++ {
		mp := b.pattern.Pattern[i]

		if mp.Initiator == b.initiator {
			if err := b.writeMsgPattern(rw, mp); err != nil {
				return err
			}
			continue
		}

		if err := b.readMsgPattern(rw, mp); err != nil {
			return err
		}
	}

	b.split()

	if b.version >= HandshakeVersion2 {
		err := b.cfg.ConnData.SetRemote(b.remoteStatic)
		if err != nil {
			return err
		}
	}

	if b.initiator {
		err := b.cfg.ConnData.SetAuthData(b.receivedPayload)
		if err != nil {
			return err
		}
	}

	return nil
}

// ecdh performs an ECDH operation between pub and priv. The returned value is
// the sha256 of the compressed shared point.
func ecdh(pub *btcec.PublicKey, priv keychain.SingleKeyECDH) ([]byte, error) {
	hash, err := priv.ECDH(pub)
	return hash[:], err
}

// hmac256 computes the HMAC-SHA256 of a key and message.
func hmac256(key, message []byte) ([]byte, error) {
	mac := hmac.New(sha256.New, key)
	_, err := mac.Write(message)
	if err != nil {
		return nil, err
	}

	return mac.Sum(nil), nil
}

// ekeMask masks the passed ephemeral key with the stored pass phrase entropy,
// using SPAKE2 as the public masking operation: me = e + N*pw
func ekeMask(e *btcec.PublicKey, passphraseEntropy []byte) *btcec.PublicKey {
	// me = e + N*pw
	passPointX, passPointY := btcec.S256().ScalarMult(
		N.X, N.Y, passphraseEntropy,
	)
	maskedEx, maskedEy := btcec.S256().Add(
		e.X, e.Y,
		passPointX, passPointY,
	)

	return &btcec.PublicKey{
		X:     maskedEx,
		Y:     maskedEy,
		Curve: btcec.S256(),
	}
}

// ekeUnmask does the inverse operation of ekeMask: e = me - N*pw
func ekeUnmask(me *btcec.PublicKey, passphraseEntropy []byte) *btcec.PublicKey {
	// First, we'll need to re-generate the passphraseEntropy point: N*pw
	passPointX, passPointY := btcec.S256().ScalarMult(
		N.X, N.Y, passphraseEntropy,
	)

	// With that generated, negate the y coordinate, then add that to the
	// masked point, which gives us the proper ephemeral key.
	passPointNegY := new(big.Int).Neg(passPointY)
	passPointNegY = passPointY.Mod(passPointNegY, btcec.S256().P)

	// e = me - N*pw
	eX, eY := btcec.S256().Add(
		me.X, me.Y,
		passPointX, passPointNegY,
	)

	return &btcec.PublicKey{
		X:     eX,
		Y:     eY,
		Curve: btcec.S256(),
	}
}

// split is the final wrap-up act to be executed at the end of a successful
// three act handshake. This function creates two internal cipherState
// instances: one which is used to encrypt messages from the initiator to the
// responder, and another which is used to encrypt message for the opposite
// direction.
func (b *Machine) split() {
	var (
		empty   []byte
		sendKey [32]byte
		recvKey [32]byte
	)

	h := hkdf.New(sha256.New, empty, b.chainingKey[:], empty)

	// If we're the initiator the first 32 bytes are used to encrypt our
	// messages and the second 32-bytes to decrypt their messages. For the
	// responder the opposite is true.
	if b.initiator {
		_, _ = h.Read(sendKey[:])
		b.sendCipher = cipherState{}
		b.sendCipher.InitializeKeyWithSalt(b.chainingKey, sendKey)

		_, _ = h.Read(recvKey[:])
		b.recvCipher = cipherState{}
		b.recvCipher.InitializeKeyWithSalt(b.chainingKey, recvKey)
	} else {
		_, _ = h.Read(recvKey[:])
		b.recvCipher = cipherState{}
		b.recvCipher.InitializeKeyWithSalt(b.chainingKey, recvKey)

		_, _ = h.Read(sendKey[:])
		b.sendCipher = cipherState{}
		b.sendCipher.InitializeKeyWithSalt(b.chainingKey, sendKey)
	}
}

// WriteMessage encrypts and buffers the next message p. The ciphertext of the
// message is prepended with an encrypt+auth'd length which must be used as the
// AD to the AEAD construction when being decrypted by the other side.
//
// NOTE: This DOES NOT write the message to the wire, it should be followed by a
// call to Flush to ensure the message is written.
func (b *Machine) WriteMessage(p []byte) error {
	// The total length of each message payload including the MAC size
	// payload exceed the largest number encodable within a 16-bit unsigned
	// integer.
	if len(p) > math.MaxUint16 {
		return ErrMaxMessageLengthExceeded
	}

	// If a prior message was written but it hasn't been fully flushed,
	// return an error as we only support buffering of one message at a
	// time.
	if len(b.nextHeaderSend) > 0 || len(b.nextBodySend) > 0 {
		return ErrMessageNotFlushed
	}

	// The full length of the packet is only the packet length, and does
	// NOT include the MAC.
	fullLength := uint16(len(p))

	var pktLen [2]byte
	binary.BigEndian.PutUint16(pktLen[:], fullLength)

	// First, generate the encrypted+MAC'd length prefix for the packet.
	b.nextHeaderSend = b.sendCipher.Encrypt(nil, nil, pktLen[:])

	// Finally, generate the encrypted packet itself.
	b.nextBodySend = b.sendCipher.Encrypt(nil, nil, p)

	return nil
}

// Flush attempts to write a message buffered using WriteMessage to the provided
// io.Writer. If no buffered message exists, this will result in a NOP.
// Otherwise, it will continue to write the remaining bytes, picking up where
// the byte stream left off in the event of a partial write. The number of bytes
// returned reflects the number of plaintext bytes in the payload, and does not
// account for the overhead of the header or MACs.
//
// NOTE: It is safe to call this method again iff a timeout error is returned.
func (b *Machine) Flush(w io.Writer) (int, error) {
	// First, write out the pending header bytes, if any exist. Any header
	// bytes written will not count towards the total amount flushed.
	if len(b.nextHeaderSend) > 0 {
		// Write any remaining header bytes and shift the slice to point
		// to the next segment of unwritten bytes. If an error is
		// encountered, we can continue to write the header from where
		// we left off on a subsequent call to Flush.
		n, err := w.Write(b.nextHeaderSend)
		b.nextHeaderSend = b.nextHeaderSend[n:]
		if err != nil {
			return 0, err
		}
	}

	// Next, write the pending body bytes, if any exist. Only the number of
	// bytes written that correspond to the ciphertext will be included in
	// the total bytes written, bytes written as part of the MAC will not be
	// counted.
	var nn int
	if len(b.nextBodySend) > 0 {
		// Write out all bytes excluding the mac and shift the body
		// slice depending on the number of actual bytes written.
		n, err := w.Write(b.nextBodySend)
		b.nextBodySend = b.nextBodySend[n:]

		// If we partially or fully wrote any of the body's MAC, we'll
		// subtract that contribution from the total amount flushed to
		// preserve the abstraction of returning the number of plaintext
		// bytes written by the connection.
		//
		// There are three possible scenarios we must handle to ensure
		// the returned value is correct. In the first case, the write
		// straddles both payload and MAC bytes, and we must subtract
		// the number of MAC bytes written from n. In the second, only
		// payload bytes are written, thus we can return n unmodified.
		// The final scenario pertains to the case where only MAC bytes
		// are written, none of which count towards the total.
		//
		//                 |-----------Payload------------|----MAC----|
		// Straddle:       S---------------------------------E--------0
		// Payload-only:   S------------------------E-----------------0
		// MAC-only:                                        S-------E-0
		start, end := n+len(b.nextBodySend), len(b.nextBodySend)
		switch {

		// Straddles payload and MAC bytes, subtract number of MAC bytes
		// written from the actual number written.
		case start > macSize && end <= macSize:
			nn = n - (macSize - end)

		// Only payload bytes are written, return n directly.
		case start > macSize && end > macSize:
			nn = n

		// Only MAC bytes are written, return 0 bytes written.
		default:
		}

		if err != nil {
			return nn, err
		}
	}

	return nn, nil
}

// ReadMessage attempts to read the next message from the passed io.Reader. In
// the case of an authentication error, a non-nil error is returned.
func (b *Machine) ReadMessage(r io.Reader) ([]byte, error) {
	pktLen, err := b.ReadHeader(r)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): need to be able to handle messages over multiple
	// protocol messages?

	buf := make([]byte, pktLen)
	return b.ReadBody(r, buf)
}

// ReadHeader attempts to read the next message header from the passed
// io.Reader. The header contains the length of the next body including
// additional overhead of the MAC. In the case of an authentication error, a
// non-nil error is returned.
//
// NOTE: This method SHOULD NOT be used in the case that the io.Reader may be
// adversarial and induce long delays. If the caller needs to set read deadlines
// appropriately, it is preferred that they use the split ReadHeader and
// ReadBody methods so that the deadlines can be set appropriately on each.
func (b *Machine) ReadHeader(r io.Reader) (uint32, error) {
	_, err := io.ReadFull(r, b.nextCipherHeader[:])
	if err != nil {
		return 0, err
	}

	// Attempt to decrypt+auth the packet length present in the stream.
	pktLenBytes, err := b.recvCipher.Decrypt(
		nil, nil, b.nextCipherHeader[:],
	)
	if err != nil {
		return 0, err
	}

	// Compute the packet length that we will need to read off the wire.
	pktLen := uint32(binary.BigEndian.Uint16(pktLenBytes)) + macSize

	return pktLen, nil
}

// ReadBody attempts to ready the next message body from the passed io.Reader.
// The provided buffer MUST be the length indicated by the packet length
// returned by the preceding call to ReadHeader. In the case of an
// authentication eerror, a non-nil error is returned.
func (b *Machine) ReadBody(r io.Reader, buf []byte) ([]byte, error) {
	// Next, using the length read from the packet header, read the
	// encrypted packet itself into the buffer allocated by the read
	// pool.
	_, err := io.ReadFull(r, buf)
	if err != nil {
		return nil, err
	}

	// Finally, decrypt the message held in the buffer, and return a
	// new byte slice containing the plaintext.
	// TODO(roasbeef): modify to let pass in slice
	return b.recvCipher.Decrypt(nil, nil, buf)
}
