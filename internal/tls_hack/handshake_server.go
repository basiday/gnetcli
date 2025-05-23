//lint:file-ignore SA1019 imported file
// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tlshack

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
)

// serverHandshakeState contains details of a server handshake in progress.
// It's discarded once the handshake has completed.
type serverHandshakeState struct {
	c                     *Conn
	clientHello           *clientHelloMsg
	hello                 *serverHelloMsg
	suite                 *cipherSuite
	ellipticOk            bool
	ecdsaOk               bool
	rsaDecryptOk          bool
	rsaSignOk             bool
	sessionState          *sessionState
	finishedHash          finishedHash
	masterSecret          []byte
	certsFromClient       [][]byte
	cert                  *Certificate
	cachedClientHelloInfo *ClientHelloInfo
}

// serverHandshake performs a TLS handshake as a server.
// c.out.Mutex <= L; c.handshakeMutex <= L.
func (c *Conn) serverHandshake() error {
	// If this is the first server handshake, we generate a random key to
	// encrypt the tickets with.
	c.config.serverInitOnce.Do(c.config.serverInit)

	//nolint:exhaustivestruct
	hs := serverHandshakeState{
		c: c,
	}
	isResume, err := hs.readClientHello()
	if err != nil {
		return err
	}

	// For an overview of TLS handshaking, see https://tools.ietf.org/html/rfc5246#section-7.3
	c.buffering = true
	if isResume {
		// The client has included a session ticket and so we do an abbreviated handshake.
		if err := hs.doResumeHandshake(); err != nil {
			return err
		}
		if err := hs.establishKeys(); err != nil {
			return err
		}
		// ticketSupported is set in a resumption handshake if the
		// ticket from the client was encrypted with an old session
		// ticket key and thus a refreshed ticket should be sent.
		if hs.hello.ticketSupported {
			if err := hs.sendSessionTicket(); err != nil {
				return err
			}
		}
		if err := hs.sendFinished(c.serverFinished[:]); err != nil {
			return err
		}
		if _, err := c.flush(); err != nil {
			return err
		}
		c.clientFinishedIsFirst = false
		if err := hs.readFinished(nil); err != nil {
			return err
		}
		c.didResume = true
	} else {
		// The client didn't include a session ticket, or it wasn't
		// valid so we do a full handshake.
		if err := hs.doFullHandshake(); err != nil {
			return err
		}
		if err := hs.establishKeys(); err != nil {
			return err
		}
		if err := hs.readFinished(c.clientFinished[:]); err != nil {
			return err
		}
		c.clientFinishedIsFirst = true
		c.buffering = true
		if err := hs.sendSessionTicket(); err != nil {
			return err
		}
		if err := hs.sendFinished(nil); err != nil {
			return err
		}
		if _, err := c.flush(); err != nil {
			return err
		}
	}
	c.handshakeComplete = true

	return nil
}

// readClientHello reads a ClientHello message from the client and decides
// whether we will perform session resumption.
func (hs *serverHandshakeState) readClientHello() (isResume bool, err error) {
	c := hs.c

	msg, err := c.readHandshake()
	if err != nil {
		return false, err
	}
	var ok bool
	hs.clientHello, ok = msg.(*clientHelloMsg)
	if !ok {
		err := c.sendAlert(alertUnexpectedMessage)
		if err != nil {
			return false, err
		}
		return false, unexpectedMessageError(hs.clientHello, msg)
	}

	if c.config.GetConfigForClient != nil {
		if newConfig, err := c.config.GetConfigForClient(hs.clientHelloInfo()); err != nil {
			err := c.sendAlert(alertInternalError)
			if err != nil {
				return false, err
			}
			return false, err
		} else if newConfig != nil {
			newConfig.mutex.Lock()
			newConfig.originalConfig = c.config
			newConfig.mutex.Unlock()

			newConfig.serverInitOnce.Do(newConfig.serverInit)
			c.config = newConfig
		}
	}

	c.vers, ok = c.config.mutualVersion(hs.clientHello.vers)
	if !ok {
		err := c.sendAlert(alertProtocolVersion)
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("tls: client offered an unsupported, maximum protocol version of %x", hs.clientHello.vers)
	}
	c.haveVers = true

	hs.hello = new(serverHelloMsg)

	supportedCurve := false
	preferredCurves := c.config.curvePreferences()
Curves:
	for _, curve := range hs.clientHello.supportedCurves {
		for _, supported := range preferredCurves {
			if supported == curve {
				supportedCurve = true
				break Curves
			}
		}
	}

	supportedPointFormat := false
	for _, pointFormat := range hs.clientHello.supportedPoints {
		if pointFormat == pointFormatUncompressed {
			supportedPointFormat = true
			break
		}
	}
	hs.ellipticOk = supportedCurve && supportedPointFormat

	foundCompression := false
	// We only support null compression, so check that the client offered it.
	for _, compression := range hs.clientHello.compressionMethods {
		if compression == compressionNone {
			foundCompression = true
			break
		}
	}

	if !foundCompression {
		err := c.sendAlert(alertHandshakeFailure)
		if err != nil {
			return false, err
		}
		return false, errors.New("tls: client does not support uncompressed connections")
	}

	hs.hello.vers = c.vers
	hs.hello.random = make([]byte, 32)
	_, err = io.ReadFull(c.config.rand(), hs.hello.random)
	if err != nil {
		err := c.sendAlert(alertInternalError)
		if err != nil {
			return false, err
		}
		return false, err
	}

	if len(hs.clientHello.secureRenegotiation) != 0 {
		err := c.sendAlert(alertHandshakeFailure)
		if err != nil {
			return false, err
		}
		return false, errors.New("tls: initial handshake had non-empty renegotiation extension")
	}

	hs.hello.secureRenegotiationSupported = hs.clientHello.secureRenegotiationSupported
	hs.hello.compressionMethod = compressionNone
	if len(hs.clientHello.serverName) > 0 {
		c.serverName = hs.clientHello.serverName
	}

	if len(hs.clientHello.alpnProtocols) > 0 {
		if selectedProto, fallback := mutualProtocol(hs.clientHello.alpnProtocols, c.config.NextProtos); !fallback {
			hs.hello.alpnProtocol = selectedProto
			c.clientProtocol = selectedProto
		}
	} else {
		// Although sending an empty NPN extension is reasonable, Firefox has
		// had a bug around this. Best to send nothing at all if
		// c.config.NextProtos is empty. See
		// https://golang.org/issue/5445.
		if hs.clientHello.nextProtoNeg && len(c.config.NextProtos) > 0 {
			hs.hello.nextProtoNeg = true
			hs.hello.nextProtos = c.config.NextProtos
		}
	}

	hs.cert, err = c.config.getCertificate(hs.clientHelloInfo())
	if err != nil {
		err := c.sendAlert(alertInternalError)
		if err != nil {
			return false, err
		}
		return false, err
	}
	if hs.cert != nil {
		if hs.clientHello.scts {
			hs.hello.scts = hs.cert.SignedCertificateTimestamps
		}

		if priv, ok := hs.cert.PrivateKey.(crypto.Signer); ok {
			switch priv.Public().(type) {
			case *ecdsa.PublicKey:
				hs.ecdsaOk = true
			case *rsa.PublicKey:
				hs.rsaSignOk = true
			default:
				err := c.sendAlert(alertInternalError)
				if err != nil {
					return false, err
				}
				return false, fmt.Errorf("tls: unsupported signing key type (%T)", priv.Public())
			}
		}
		if priv, ok := hs.cert.PrivateKey.(crypto.Decrypter); ok {
			switch priv.Public().(type) {
			case *rsa.PublicKey:
				hs.rsaDecryptOk = true
			default:
				err := c.sendAlert(alertInternalError)
				if err != nil {
					return false, err
				}
				return false, fmt.Errorf("tls: unsupported decryption key type (%T)", priv.Public())
			}
		}
	}

	var preferenceList, supportedList []uint16
	if c.config.PreferServerCipherSuites {
		preferenceList = c.config.cipherSuites()
		supportedList = hs.clientHello.cipherSuites
	} else {
		preferenceList = hs.clientHello.cipherSuites
		supportedList = c.config.cipherSuites()
	}

	for _, id := range preferenceList {
		if hs.setCipherSuite(id, supportedList, c.vers) {
			break
		}
	}

	if hs.suite == nil {
		err := c.sendAlert(alertHandshakeFailure)
		if err != nil {
			return false, err
		}
		return false, errors.New("tls: no cipher suite supported by both client and server")
	}

	// See https://tools.ietf.org/html/rfc7507.
	for _, id := range hs.clientHello.cipherSuites {
		if id == TLS_FALLBACK_SCSV {
			// The client is doing a fallback connection.
			if hs.clientHello.vers < c.config.maxVersion() {
				err := c.sendAlert(alertInappropriateFallback)
				if err != nil {
					return false, err
				}
				return false, errors.New("tls: client using inappropriate protocol fallback")
			}
			break
		}
	}

	if hs.checkForResumption() {
		return true, nil
	}

	return false, nil
}

// checkForResumption reports whether we should perform resumption on this connection.
func (hs *serverHandshakeState) checkForResumption() bool {
	c := hs.c

	if c.config.SessionTicketsDisabled {
		return false
	}

	var ok bool
	var sessionTicket = append([]uint8{}, hs.clientHello.sessionTicket...)
	if hs.sessionState, ok = c.decryptTicket(sessionTicket); !ok {
		return false
	}

	// Never resume a session for a different TLS version.
	if c.vers != hs.sessionState.vers {
		return false
	}

	cipherSuiteOk := false
	// Check that the client is still offering the ciphersuite in the session.
	for _, id := range hs.clientHello.cipherSuites {
		if id == hs.sessionState.cipherSuite {
			cipherSuiteOk = true
			break
		}
	}
	if !cipherSuiteOk {
		return false
	}

	// Check that we also support the ciphersuite from the session.
	if !hs.setCipherSuite(hs.sessionState.cipherSuite, c.config.cipherSuites(), hs.sessionState.vers) {
		return false
	}

	sessionHasClientCerts := len(hs.sessionState.certificates) != 0
	needClientCerts := c.config.ClientAuth == RequireAnyClientCert || c.config.ClientAuth == RequireAndVerifyClientCert
	if needClientCerts && !sessionHasClientCerts {
		return false
	}
	if sessionHasClientCerts && c.config.ClientAuth == NoClientCert {
		return false
	}

	return true
}

func (hs *serverHandshakeState) doResumeHandshake() error {
	c := hs.c

	hs.hello.cipherSuite = hs.suite.id
	// We echo the client's session ID in the ServerHello to let it know
	// that we're doing a resumption.
	hs.hello.sessionID = hs.clientHello.sessionID
	hs.hello.ticketSupported = hs.sessionState.usedOldKey
	hs.finishedHash = newFinishedHash(c.vers, hs.suite)
	hs.finishedHash.discardHandshakeBuffer()
	_, err := hs.finishedHash.Write(hs.clientHello.marshal())
	if err != nil {
		return err
	}
	_, err = hs.finishedHash.Write(hs.hello.marshal())
	if err != nil {
		return err
	}
	if _, err := c.writeRecord(recordTypeHandshake, hs.hello.marshal()); err != nil {
		return err
	}

	if len(hs.sessionState.certificates) > 0 {
		if _, err := hs.processCertsFromClient(hs.sessionState.certificates); err != nil {
			return err
		}
	}

	hs.masterSecret = hs.sessionState.masterSecret

	return nil
}

func (hs *serverHandshakeState) doFullHandshake() error {
	c := hs.c

	if hs.cert != nil && hs.clientHello.ocspStapling && len(hs.cert.OCSPStaple) > 0 {
		hs.hello.ocspStapling = true
	}

	hs.hello.ticketSupported = hs.clientHello.ticketSupported && !c.config.SessionTicketsDisabled
	hs.hello.cipherSuite = hs.suite.id

	hs.finishedHash = newFinishedHash(hs.c.vers, hs.suite)
	if c.config.ClientAuth == NoClientCert {
		// No need to keep a full record of the handshake if client
		// certificates won't be used.
		hs.finishedHash.discardHandshakeBuffer()
	}
	_, err := hs.finishedHash.Write(hs.clientHello.marshal())
	if err != nil {
		return err
	}
	_, err = hs.finishedHash.Write(hs.hello.marshal())
	if err != nil {
		return err
	}
	if _, err := c.writeRecord(recordTypeHandshake, hs.hello.marshal()); err != nil {
		return err
	}

	certMsg := new(certificateMsg)
	if hs.suite.flags&suiteNoCerts == 0 {
		certMsg.certificates = hs.cert.Certificate
		_, err := hs.finishedHash.Write(certMsg.marshal())
		if err != nil {
			return err
		}
		if _, err := c.writeRecord(recordTypeHandshake, certMsg.marshal()); err != nil {
			return err
		}
	}

	if hs.hello.ocspStapling {
		certStatus := new(certificateStatusMsg)
		certStatus.statusType = statusTypeOCSP
		certStatus.response = hs.cert.OCSPStaple
		_, err := hs.finishedHash.Write(certStatus.marshal())
		if err != nil {
			return err
		}
		if _, err := c.writeRecord(recordTypeHandshake, certStatus.marshal()); err != nil {
			return err
		}
	}

	keyAgreement := hs.suite.ka(c.vers)
	skx, err := keyAgreement.generateServerKeyExchange(c.config, hs.cert, hs.clientHello, hs.hello)
	if err != nil {
		err := c.sendAlert(alertHandshakeFailure)
		if err != nil {
			return err
		}
		return err
	}
	if skx != nil {
		_, err := hs.finishedHash.Write(skx.marshal())
		if err != nil {
			return err
		}
		if _, err := c.writeRecord(recordTypeHandshake, skx.marshal()); err != nil {
			return err
		}
	}

	if c.config.ClientAuth >= RequestClientCert {
		// Request a client certificate
		certReq := new(certificateRequestMsg)
		certReq.certificateTypes = []byte{
			byte(certTypeRSASign),
			byte(certTypeECDSASign),
		}
		if c.vers >= VersionTLS12 {
			certReq.hasSignatureAndHash = true
			certReq.signatureAndHashes = supportedSignatureAlgorithms
		}

		// An empty list of certificateAuthorities signals to
		// the client that it may send any certificate in response
		// to our request. When we know the CAs we trust, then
		// we can send them down, so that the client can choose
		// an appropriate certificate to give to us.
		if c.config.ClientCAs != nil {
			certReq.certificateAuthorities = c.config.ClientCAs.Subjects()
		}
		_, err := hs.finishedHash.Write(certReq.marshal())
		if err != nil {
			return err
		}
		if _, err := c.writeRecord(recordTypeHandshake, certReq.marshal()); err != nil {
			return err
		}
	}

	helloDone := new(serverHelloDoneMsg)
	_, err = hs.finishedHash.Write(helloDone.marshal())
	if err != nil {
		return err
	}
	if _, err := c.writeRecord(recordTypeHandshake, helloDone.marshal()); err != nil {
		return err
	}

	if _, err := c.flush(); err != nil {
		return err
	}

	var pub crypto.PublicKey // public key for client auth, if any

	msg, err := c.readHandshake()
	if err != nil {
		return err
	}

	var ok bool
	// If we requested a client certificate, then the client must send a
	// certificate message, even if it's empty.
	if c.config.ClientAuth >= RequestClientCert {
		if certMsg, ok = msg.(*certificateMsg); !ok {
			err := c.sendAlert(alertUnexpectedMessage)
			if err != nil {
				return err
			}
			return unexpectedMessageError(certMsg, msg)
		}
		_, err := hs.finishedHash.Write(certMsg.marshal())
		if err != nil {
			return err
		}

		if len(certMsg.certificates) == 0 {
			// The client didn't actually send a certificate
			switch c.config.ClientAuth {
			case RequireAnyClientCert, RequireAndVerifyClientCert:
				err := c.sendAlert(alertBadCertificate)
				if err != nil {
					return err
				}
				return errors.New("tls: client didn't provide a certificate")
			}
		}

		pub, err = hs.processCertsFromClient(certMsg.certificates)
		if err != nil {
			return err
		}

		msg, err = c.readHandshake()
		if err != nil {
			return err
		}
	}

	// Get client key exchange
	ckx, ok := msg.(*clientKeyExchangeMsg)
	if !ok {
		err := c.sendAlert(alertUnexpectedMessage)
		if err != nil {
			return err
		}
		return unexpectedMessageError(ckx, msg)
	}
	_, err = hs.finishedHash.Write(ckx.marshal())
	if err != nil {
		return err
	}

	preMasterSecret, err := keyAgreement.processClientKeyExchange(c.config, hs.cert, ckx, c.vers)
	if err != nil {
		err := c.sendAlert(alertHandshakeFailure)
		if err != nil {
			return err
		}
		return err
	}
	hs.masterSecret = masterFromPreMasterSecret(c.vers, hs.suite, preMasterSecret, hs.clientHello.random, hs.hello.random)
	if err := c.config.writeKeyLog(hs.clientHello.random, hs.masterSecret); err != nil {
		err := c.sendAlert(alertInternalError)
		if err != nil {
			return err
		}
		return err
	}

	// If we received a client cert in response to our certificate request message,
	// the client will send us a certificateVerifyMsg immediately after the
	// clientKeyExchangeMsg. This message is a digest of all preceding
	// handshake-layer messages that is signed using the private key corresponding
	// to the client's certificate. This allows us to verify that the client is in
	// possession of the private key of the certificate.
	if len(c.peerCertificates) > 0 {
		msg, err = c.readHandshake()
		if err != nil {
			return err
		}
		certVerify, ok := msg.(*certificateVerifyMsg)
		if !ok {
			err := c.sendAlert(alertUnexpectedMessage)
			if err != nil {
				return err
			}
			return unexpectedMessageError(certVerify, msg)
		}

		// Determine the signature type.
		var signatureAndHash signatureAndHash
		if certVerify.hasSignatureAndHash {
			signatureAndHash = certVerify.signatureAndHash
			if !isSupportedSignatureAndHash(signatureAndHash, supportedSignatureAlgorithms) {
				return errors.New("tls: unsupported hash function for client certificate")
			}
		} else {
			// Before TLS 1.2 the signature algorithm was implicit
			// from the key type, and only one hash per signature
			// algorithm was possible. Leave the hash as zero.
			switch pub.(type) {
			case *ecdsa.PublicKey:
				signatureAndHash.signature = signatureECDSA
			case *rsa.PublicKey:
				signatureAndHash.signature = signatureRSA
			}
		}

		switch key := pub.(type) {
		case *ecdsa.PublicKey:
			if signatureAndHash.signature != signatureECDSA {
				err = errors.New("tls: bad signature type for client's ECDSA certificate")
				break
			}
			ecdsaSig := new(ecdsaSignature)
			if _, err = asn1.Unmarshal(certVerify.signature, ecdsaSig); err != nil {
				break
			}
			if ecdsaSig.R.Sign() <= 0 || ecdsaSig.S.Sign() <= 0 {
				err = errors.New("tls: ECDSA signature contained zero or negative values")
				break
			}
			var digest []byte
			if digest, _, err = hs.finishedHash.hashForClientCertificate(signatureAndHash, hs.masterSecret); err != nil {
				break
			}
			if !ecdsa.Verify(key, digest, ecdsaSig.R, ecdsaSig.S) {
				err = errors.New("tls: ECDSA verification failure")
			}
		case *rsa.PublicKey:
			if signatureAndHash.signature != signatureRSA {
				err = errors.New("tls: bad signature type for client's RSA certificate")
				break
			}
			var digest []byte
			var hashFunc crypto.Hash
			if digest, hashFunc, err = hs.finishedHash.hashForClientCertificate(signatureAndHash, hs.masterSecret); err != nil {
				break
			}
			err = rsa.VerifyPKCS1v15(key, hashFunc, digest, certVerify.signature)
		}
		if err != nil {
			errAlert := c.sendAlert(alertBadCertificate)
			if errAlert != nil {
				return errAlert
			}
			return errors.New("tls: could not validate signature of connection nonces: " + err.Error())
		}

		_, err := hs.finishedHash.Write(certVerify.marshal())
		if err != nil {
			return err
		}
	}

	hs.finishedHash.discardHandshakeBuffer()

	return nil
}

func (hs *serverHandshakeState) establishKeys() error {
	c := hs.c

	clientMAC, serverMAC, clientKey, serverKey, clientIV, serverIV :=
		keysFromMasterSecret(c.vers, hs.suite, hs.masterSecret, hs.clientHello.random, hs.hello.random, hs.suite.macLen, hs.suite.keyLen, hs.suite.ivLen)

	var clientCipher, serverCipher interface{}
	var clientHash, serverHash macFunction

	if hs.suite.aead == nil {
		clientCipher = hs.suite.cipher(clientKey, clientIV, true /* for reading */)
		clientHash = hs.suite.mac(c.vers, clientMAC)
		serverCipher = hs.suite.cipher(serverKey, serverIV, false /* not for reading */)
		serverHash = hs.suite.mac(c.vers, serverMAC)
	} else {
		clientCipher = hs.suite.aead(clientKey, clientIV)
		serverCipher = hs.suite.aead(serverKey, serverIV)
	}

	c.in.prepareCipherSpec(c.vers, clientCipher, clientHash)
	c.out.prepareCipherSpec(c.vers, serverCipher, serverHash)

	return nil
}

func (hs *serverHandshakeState) readFinished(out []byte) error {
	c := hs.c

	err := c.readRecord(recordTypeChangeCipherSpec)
	if err != nil {
		return err
	}
	if c.in.err != nil {
		return c.in.err
	}

	if hs.hello.nextProtoNeg {
		msg, err := c.readHandshake()
		if err != nil {
			return err
		}
		nextProto, ok := msg.(*nextProtoMsg)
		if !ok {
			err := c.sendAlert(alertUnexpectedMessage)
			if err != nil {
				return err
			}
			return unexpectedMessageError(nextProto, msg)
		}
		_, err = hs.finishedHash.Write(nextProto.marshal())
		if err != nil {
			return err
		}
		c.clientProtocol = nextProto.proto
	}

	msg, err := c.readHandshake()
	if err != nil {
		return err
	}
	clientFinished, ok := msg.(*finishedMsg)
	if !ok {
		err := c.sendAlert(alertUnexpectedMessage)
		if err != nil {
			return err
		}
		return unexpectedMessageError(clientFinished, msg)
	}

	verify := hs.finishedHash.clientSum(hs.masterSecret)
	if len(verify) != len(clientFinished.verifyData) ||
		subtle.ConstantTimeCompare(verify, clientFinished.verifyData) != 1 {
		err := c.sendAlert(alertHandshakeFailure)
		if err != nil {
			return err
		}
		return errors.New("tls: client's Finished message is incorrect")
	}

	_, err = hs.finishedHash.Write(clientFinished.marshal())
	if err != nil {
		return err
	}
	copy(out, verify)
	return nil
}

func (hs *serverHandshakeState) sendSessionTicket() error {
	if !hs.hello.ticketSupported {
		return nil
	}

	c := hs.c
	m := new(newSessionTicketMsg)

	var err error
	//nolint:exhaustivestruct
	state := sessionState{
		vers:         c.vers,
		cipherSuite:  hs.suite.id,
		masterSecret: hs.masterSecret,
		certificates: hs.certsFromClient,
	}
	m.ticket, err = c.encryptTicket(&state)
	if err != nil {
		return err
	}

	_, err = hs.finishedHash.Write(m.marshal())
	if err != nil {
		return err
	}
	if _, err := c.writeRecord(recordTypeHandshake, m.marshal()); err != nil {
		return err
	}

	return nil
}

func (hs *serverHandshakeState) sendFinished(out []byte) error {
	c := hs.c

	if _, err := c.writeRecord(recordTypeChangeCipherSpec, []byte{1}); err != nil {
		return err
	}

	finished := new(finishedMsg)
	finished.verifyData = hs.finishedHash.serverSum(hs.masterSecret)
	_, err := hs.finishedHash.Write(finished.marshal())
	if err != nil {
		return err
	}
	if _, err := c.writeRecord(recordTypeHandshake, finished.marshal()); err != nil {
		return err
	}

	c.cipherSuite = hs.suite.id
	copy(out, finished.verifyData)

	return nil
}

// processCertsFromClient takes a chain of client certificates either from a
// Certificates message or from a sessionState and verifies them. It returns
// the public key of the leaf certificate.
func (hs *serverHandshakeState) processCertsFromClient(certificates [][]byte) (crypto.PublicKey, error) {
	c := hs.c

	hs.certsFromClient = certificates
	certs := make([]*x509.Certificate, len(certificates))
	var err error
	for i, asn1Data := range certificates {
		if certs[i], err = x509.ParseCertificate(asn1Data); err != nil {
			errAlert := c.sendAlert(alertBadCertificate)
			if errAlert != nil {
				return nil, errAlert
			}
			return nil, errors.New("tls: failed to parse client certificate: " + err.Error())
		}
	}

	if c.config.ClientAuth >= VerifyClientCertIfGiven && len(certs) > 0 {
		opts := x509.VerifyOptions{
			Roots:         c.config.ClientCAs,
			CurrentTime:   c.config.time(),
			Intermediates: x509.NewCertPool(),
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}

		for _, cert := range certs[1:] {
			opts.Intermediates.AddCert(cert)
		}

		chains, err := certs[0].Verify(opts)
		if err != nil {
			errAlert := c.sendAlert(alertBadCertificate)
			if errAlert != nil {
				return nil, errAlert
			}
			return nil, errors.New("tls: failed to verify client's certificate: " + err.Error())
		}

		c.verifiedChains = chains
	}

	if c.config.VerifyPeerCertificate != nil {
		if err := c.config.VerifyPeerCertificate(certificates, c.verifiedChains); err != nil {
			err := c.sendAlert(alertBadCertificate)
			if err != nil {
				return nil, err
			}
			return nil, err
		}
	}

	if len(certs) == 0 {
		return nil, nil
	}

	var pub crypto.PublicKey
	switch key := certs[0].PublicKey.(type) {
	case *ecdsa.PublicKey, *rsa.PublicKey:
		pub = key
	default:
		err := c.sendAlert(alertUnsupportedCertificate)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("tls: client's certificate contains an unsupported public key of type %T", certs[0].PublicKey)
	}
	c.peerCertificates = certs
	return pub, nil
}

// setCipherSuite sets a cipherSuite with the given id as the serverHandshakeState
// suite if that cipher suite is acceptable to use.
// It returns a bool indicating if the suite was set.
func (hs *serverHandshakeState) setCipherSuite(id uint16, supportedCipherSuites []uint16, version uint16) bool {
	for _, supported := range supportedCipherSuites {
		if id == supported {
			var candidate *cipherSuite

			for _, s := range cipherSuites {
				if s.id == id {
					candidate = s
					break
				}
			}
			if candidate == nil {
				continue
			}
			// Don't select a ciphersuite which we can't
			// support for this client.
			if candidate.flags&suiteECDHE != 0 {
				if !hs.ellipticOk {
					continue
				}
				if candidate.flags&suiteECDSA != 0 {
					if !hs.ecdsaOk {
						continue
					}
				}
			}
			if candidate.flags&suiteRSA != 0 {
				if !hs.rsaDecryptOk {
					continue
				} else if !hs.rsaSignOk {
					continue
				}
			}
			// If DH Parameters weren't configured, can't use DHE
			if candidate.flags&suiteDHE != 0 {
				if hs.c.config.DhParameters == nil {
					continue
				}
			}
			if version < VersionTLS12 && candidate.flags&suiteTLS12 != 0 {
				continue
			}
			hs.suite = candidate
			return true
		}
	}
	return false
}

// suppVersArray is the backing array of ClientHelloInfo.SupportedVersions
var suppVersArray = [...]uint16{VersionTLS12, VersionTLS11, VersionTLS10, VersionSSL30}

func (hs *serverHandshakeState) clientHelloInfo() *ClientHelloInfo {
	if hs.cachedClientHelloInfo != nil {
		return hs.cachedClientHelloInfo
	}

	var supportedVersions []uint16
	if hs.clientHello.vers > VersionTLS12 {
		supportedVersions = suppVersArray[:]
	} else if hs.clientHello.vers >= VersionSSL30 {
		supportedVersions = suppVersArray[VersionTLS12-hs.clientHello.vers:]
	}

	signatureSchemes := make([]SignatureScheme, 0, len(hs.clientHello.signatureAndHashes))
	for _, sah := range hs.clientHello.signatureAndHashes {
		signatureSchemes = append(signatureSchemes, SignatureScheme(sah.hash)<<8+SignatureScheme(sah.signature))
	}

	hs.cachedClientHelloInfo = &ClientHelloInfo{
		CipherSuites:      hs.clientHello.cipherSuites,
		ServerName:        hs.clientHello.serverName,
		SupportedCurves:   hs.clientHello.supportedCurves,
		SupportedPoints:   hs.clientHello.supportedPoints,
		SignatureSchemes:  signatureSchemes,
		SupportedProtos:   hs.clientHello.alpnProtocols,
		SupportedVersions: supportedVersions,
		Conn:              hs.c.conn,
	}

	return hs.cachedClientHelloInfo
}
