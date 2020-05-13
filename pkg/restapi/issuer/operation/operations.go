/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package operation

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/btcsuite/btcutil/base58"
	"github.com/google/tink/go/keyset"
	"github.com/gorilla/mux"
	ariescrypto "github.com/hyperledger/aries-framework-go/pkg/crypto"
	"github.com/hyperledger/aries-framework-go/pkg/doc/jose"
	"github.com/hyperledger/aries-framework-go/pkg/doc/verifiable"
	vdriapi "github.com/hyperledger/aries-framework-go/pkg/framework/aries/api/vdri"
	"github.com/hyperledger/aries-framework-go/pkg/kms"
	"github.com/hyperledger/aries-framework-go/pkg/kms/localkms"
	ariesstorage "github.com/hyperledger/aries-framework-go/pkg/storage"
	log "github.com/sirupsen/logrus"
	"github.com/trustbloc/edge-core/pkg/storage"
	"github.com/trustbloc/edv/pkg/restapi/edv/edverrors"
	"github.com/trustbloc/edv/pkg/restapi/edv/models"

	"github.com/trustbloc/edge-service/pkg/doc/vc/crypto"
	vcprofile "github.com/trustbloc/edge-service/pkg/doc/vc/profile"
	cslstatus "github.com/trustbloc/edge-service/pkg/doc/vc/status/csl"
	"github.com/trustbloc/edge-service/pkg/internal/common/support"
	"github.com/trustbloc/edge-service/pkg/internal/cryptosetup"
	commondid "github.com/trustbloc/edge-service/pkg/restapi/internal/common/did"
	commhttp "github.com/trustbloc/edge-service/pkg/restapi/internal/common/http"
	"github.com/trustbloc/edge-service/pkg/restapi/model"
)

const (
	profileIDPathParam = "profileID"

	// issuer endpoints
	createProfileEndpoint          = "/profile"
	getProfileEndpoint             = createProfileEndpoint + "/{id}"
	storeCredentialEndpoint        = "/store"
	retrieveCredentialEndpoint     = "/retrieve"
	credentialStatus               = "/status"
	updateCredentialStatusEndpoint = "/updateStatus"
	credentialStatusEndpoint       = credentialStatus + "/{id}"
	credentialsBasePath            = "/" + "{" + profileIDPathParam + "}" + "/credentials"
	issueCredentialPath            = credentialsBasePath + "/issueCredential"
	composeAndIssueCredentialPath  = credentialsBasePath + "/composeAndIssueCredential"
	kmsBasePath                    = "/kms"
	generateKeypairPath            = kmsBasePath + "/generatekeypair"

	cslSize = 50

	invalidRequestErrMsg = "Invalid request"

	// supported proof purpose
	assertionMethod      = "assertionMethod"
	authentication       = "authentication"
	capabilityDelegation = "capabilityDelegation"
	capabilityInvocation = "capabilityInvocation"

	jsonWebSignature2020Context = "https://trustbloc.github.io/context/vc/credentials-v1.jsonld"
)

var errProfileNotFound = errors.New("specified profile ID does not exist")

var errMultipleInconsistentVCsFoundForOneID = errors.New("multiple VCs with " +
	"differing contents were found matching the given ID. This indicates inconsistency in " +
	"the VC database. To solve this, delete the extra VCs and leave only one")

// Handler http handler for each controller API endpoint
type Handler interface {
	Path() string
	Method() string
	Handle() http.HandlerFunc
}

type vcStatusManager interface {
	CreateStatusID() (*verifiable.TypedID, error)
	UpdateVCStatus(v *verifiable.Credential, profile *vcprofile.DataProfile, status, statusReason string) error
	GetCSL(id string) (*cslstatus.CSL, error)
}

// EDVClient interface to interact with edv client
type EDVClient interface {
	CreateDataVault(config *models.DataVaultConfiguration) (string, error)
	CreateDocument(vaultID string, document *models.EncryptedDocument) (string, error)
	ReadDocument(vaultID, docID string) (*models.EncryptedDocument, error)
	QueryVault(vaultID string, query *models.Query) ([]string, error)
}

type keyManager interface {
	kms.KeyManager
	ExportPubKeyBytes(id string) ([]byte, error)
	ImportPrivateKey(privKey interface{}, kt kms.KeyType, opts ...localkms.PrivateKeyOpts) (string, *keyset.Handle, error)
}

type commonDID interface {
	CreateDID(keyType, signatureType, did, privateKey, keyID, purpose string,
		registrar model.UNIRegistrar) (string, string, error)
}

// New returns CreateCredential instance
func New(config *Config) (*Operation, error) {
	c := crypto.New(config.KeyManager, config.Crypto, config.VDRI)

	vcStatusManager, err := cslstatus.New(config.StoreProvider, config.HostURL+credentialStatus, cslSize, c)
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate new csl status: %w", err)
	}

	jweEncrypter, jweDecrypter, err := cryptosetup.PrepareJWECrypto(config.KeyManager, config.StoreProvider,
		jose.A256GCM, kms.ECDHES256AES256GCMType)
	if err != nil {
		return nil, err
	}

	kh, vcIDIndexNameMACEncoded, err :=
		cryptosetup.PrepareMACCrypto(config.KeyManager, config.StoreProvider, config.Crypto, kms.HMACSHA256Tag256Type)
	if err != nil {
		return nil, err
	}

	p, err := vcprofile.New(config.StoreProvider)
	if err != nil {
		return nil, err
	}

	svc := &Operation{
		profileStore:         p,
		edvClient:            config.EDVClient,
		kms:                  config.KeyManager,
		vdri:                 config.VDRI,
		crypto:               c,
		jweEncrypter:         jweEncrypter,
		jweDecrypter:         jweDecrypter,
		vcStatusManager:      vcStatusManager,
		domain:               config.Domain,
		HostURL:              config.HostURL,
		macKeyHandle:         kh,
		macCrypto:            config.Crypto,
		vcIDIndexNameEncoded: vcIDIndexNameMACEncoded,
		commonDID: commondid.New(&commondid.Config{VDRI: config.VDRI, KeyManager: config.KeyManager,
			Domain: config.Domain, TLSConfig: config.TLSConfig}),
	}

	return svc, nil
}

// Config defines configuration for vcs operations
type Config struct {
	StoreProvider      storage.Provider
	KMSSecretsProvider ariesstorage.Provider
	EDVClient          EDVClient
	KeyManager         keyManager
	VDRI               vdriapi.Registry
	HostURL            string
	Domain             string
	TLSConfig          *tls.Config
	Crypto             ariescrypto.Crypto
}

// Operation defines handlers for Edge service
type Operation struct {
	profileStore         *vcprofile.Profile
	edvClient            EDVClient
	kms                  keyManager
	vdri                 vdriapi.Registry
	crypto               *crypto.Crypto
	jweEncrypter         jose.Encrypter
	jweDecrypter         jose.Decrypter
	vcStatusManager      vcStatusManager
	domain               string
	HostURL              string
	macKeyHandle         *keyset.Handle
	macCrypto            ariescrypto.Crypto
	vcIDIndexNameEncoded string
	commonDID            commonDID
}

// GetRESTHandlers get all controller API handler available for this service
func (o *Operation) GetRESTHandlers() []Handler {
	return []Handler{
		// issuer profile
		support.NewHTTPHandler(createProfileEndpoint, http.MethodPost, o.createIssuerProfileHandler),
		support.NewHTTPHandler(getProfileEndpoint, http.MethodGet, o.getIssuerProfileHandler),

		// verifiable credential store
		support.NewHTTPHandler(storeCredentialEndpoint, http.MethodPost, o.storeCredentialHandler),
		support.NewHTTPHandler(retrieveCredentialEndpoint, http.MethodGet, o.retrieveCredentialHandler),

		// verifiable credential status
		support.NewHTTPHandler(updateCredentialStatusEndpoint, http.MethodPost, o.updateCredentialStatusHandler),
		support.NewHTTPHandler(credentialStatusEndpoint, http.MethodGet, o.retrieveCredentialStatus),

		// issuer apis
		support.NewHTTPHandler(generateKeypairPath, http.MethodGet, o.generateKeypairHandler),
		support.NewHTTPHandler(issueCredentialPath, http.MethodPost, o.issueCredentialHandler),
		support.NewHTTPHandler(composeAndIssueCredentialPath, http.MethodPost, o.composeAndIssueCredentialHandler),
	}
}

// RetrieveCredentialStatus swagger:route GET /status/{id} issuer retrieveCredentialStatusReq
//
// Retrieves the credential status.
//
// Responses:
//    default: genericError
//        200: retrieveCredentialStatusResp
func (o *Operation) retrieveCredentialStatus(rw http.ResponseWriter, req *http.Request) {
	csl, err := o.vcStatusManager.GetCSL(o.HostURL + req.RequestURI)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest,
			fmt.Sprintf("failed to get credential status list: %s", err.Error()))

		return
	}

	rw.WriteHeader(http.StatusOK)
	commhttp.WriteResponse(rw, csl)
}

// UpdateCredentialStatus swagger:route POST /updateStatus issuer updateCredentialStatusReq
//
// Updates credential status.
//
// Responses:
//    default: genericError
//        200: emptyRes
func (o *Operation) updateCredentialStatusHandler(rw http.ResponseWriter, req *http.Request) {
	data := UpdateCredentialStatusRequest{}
	err := json.NewDecoder(req.Body).Decode(&data)

	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest,
			fmt.Sprintf("failed to decode request received: %s", err.Error()))
		return
	}

	// TODO https://github.com/trustbloc/edge-service/issues/208 credential is bundled into string type - update
	//  this to json.RawMessage
	vc, err := o.parseAndVerifyVC([]byte(data.Credential))
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest,
			fmt.Sprintf("unable to unmarshal the VC: %s", err.Error()))
		return
	}

	// get profile
	profile, err := o.profileStore.GetProfile(vc.Issuer.CustomFields["name"].(string))
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest,
			fmt.Sprintf("failed to get profile: %s", err.Error()))
		return
	}

	if profile.DisableVCStatus {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest,
			fmt.Sprintf("vc status is disabled for profile %s", profile.Name))
		return
	}

	if err := o.vcStatusManager.UpdateVCStatus(vc, profile, data.Status, data.StatusReason); err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest,
			fmt.Sprintf("failed to update vc status: %s", err.Error()))
		return
	}

	rw.WriteHeader(http.StatusOK)
}

// CreateIssuerProfile swagger:route POST /profile issuer issuerProfileReq
//
// Creates issuer profile.
//
// Responses:
//    default: genericError
//        201: issuerProfileRes
func (o *Operation) createIssuerProfileHandler(rw http.ResponseWriter, req *http.Request) {
	data := ProfileRequest{}

	if err := json.NewDecoder(req.Body).Decode(&data); err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, fmt.Sprintf(invalidRequestErrMsg+": %s", err.Error()))

		return
	}

	if err := validateProfileRequest(&data); err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, err.Error())

		return
	}

	profile, err := o.createIssuerProfile(&data)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, err.Error())

		return
	}

	err = o.profileStore.SaveProfile(profile)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, err.Error())

		return
	}

	// create the vault associated with the profile
	_, err = o.edvClient.CreateDataVault(&models.DataVaultConfiguration{ReferenceID: profile.Name})
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, err.Error())

		return
	}

	rw.WriteHeader(http.StatusCreated)
	commhttp.WriteResponse(rw, profile)
}

// RetrieveIssuerProfile swagger:route GET /profile/{id} issuer retrieveProfileReq
//
// Retrieves issuer profile.
//
// Responses:
//    default: genericError
//        200: issuerProfileRes
func (o *Operation) getIssuerProfileHandler(rw http.ResponseWriter, req *http.Request) {
	profileID := mux.Vars(req)["id"]

	profileResponseJSON, err := o.profileStore.GetProfile(profileID)
	if err != nil {
		if errors.Is(err, errProfileNotFound) {
			commhttp.WriteErrorResponse(rw, http.StatusNotFound, "Failed to find the profile")

			return
		}

		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, err.Error())

		return
	}

	commhttp.WriteResponse(rw, profileResponseJSON)
}

// StoreVerifiableCredential swagger:route POST /store issuer storeCredentialReq
//
// Stores a credential.
//
// Responses:
//    default: genericError
//        200: emptyRes
func (o *Operation) storeCredentialHandler(rw http.ResponseWriter, req *http.Request) {
	data := &StoreVCRequest{}

	err := json.NewDecoder(req.Body).Decode(&data)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, fmt.Sprintf(invalidRequestErrMsg+": %s", err.Error()))

		return
	}

	// TODO https://github.com/trustbloc/edge-service/issues/208 credential is bundled into string type - update
	//  this to json.RawMessage
	vc, err := o.parseAndVerifyVC([]byte(data.Credential))
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest,
			fmt.Sprintf("unable to unmarshal the VC: %s", err.Error()))
		return
	}

	// TODO https://github.com/trustbloc/edge-service/issues/417 add profileID to the path param rather than the body
	if err = validateRequest(data.Profile, vc.ID); err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, err.Error())

		return
	}

	o.storeVC(data, vc, rw)
}

// ToDo: data.Credential and vc seem to contain the same data... do they both need to be passed in?
// https://github.com/trustbloc/edge-service/issues/265
func (o *Operation) storeVC(data *StoreVCRequest, vc *verifiable.Credential, rw http.ResponseWriter) {
	doc, err := o.buildStructuredDoc(data)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, err.Error())

		return
	}

	encryptedDocument, err := o.buildEncryptedDoc(doc, vc.ID)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusInternalServerError, err.Error())

		return
	}

	_, err = o.edvClient.CreateDocument(data.Profile, &encryptedDocument)

	if err != nil && strings.Contains(err.Error(), edverrors.ErrVaultNotFound.Error()) {
		// create the new vault for this profile, if it doesn't exist
		_, err = o.edvClient.CreateDataVault(&models.DataVaultConfiguration{ReferenceID: data.Profile})
		if err == nil {
			_, err = o.edvClient.CreateDocument(data.Profile, &encryptedDocument)
		}
	}

	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusInternalServerError, err.Error())

		return
	}
}

func (o *Operation) buildStructuredDoc(data *StoreVCRequest) (*models.StructuredDocument, error) {
	edvDocID, err := generateEDVCompatibleID()
	if err != nil {
		return nil, err
	}

	doc := models.StructuredDocument{}
	doc.ID = edvDocID
	doc.Content = make(map[string]interface{})

	credentialBytes := []byte(data.Credential)

	var credentialJSONRawMessage json.RawMessage = credentialBytes

	doc.Content["message"] = credentialJSONRawMessage

	return &doc, nil
}

func (o *Operation) buildEncryptedDoc(structuredDoc *models.StructuredDocument,
	vcID string) (models.EncryptedDocument, error) {
	marshalledStructuredDoc, err := json.Marshal(structuredDoc)
	if err != nil {
		return models.EncryptedDocument{}, err
	}

	jwe, err := o.jweEncrypter.Encrypt(marshalledStructuredDoc, nil)
	if err != nil {
		return models.EncryptedDocument{}, err
	}

	encryptedStructuredDoc, err := jwe.Serialize(json.Marshal)
	if err != nil {
		return models.EncryptedDocument{}, err
	}

	vcIDMAC, err := o.macCrypto.ComputeMAC([]byte(vcID), o.macKeyHandle)
	if err != nil {
		return models.EncryptedDocument{}, err
	}

	vcIDIndexValueEncoded := base64.URLEncoding.EncodeToString(vcIDMAC)

	indexedAttribute := models.IndexedAttribute{
		Name:   o.vcIDIndexNameEncoded,
		Value:  vcIDIndexValueEncoded,
		Unique: true,
	}

	indexedAttributeCollection := models.IndexedAttributeCollection{
		Sequence:          0,
		HMAC:              models.IDTypePair{},
		IndexedAttributes: []models.IndexedAttribute{indexedAttribute},
	}

	indexedAttributeCollections := []models.IndexedAttributeCollection{indexedAttributeCollection}

	encryptedDocument := models.EncryptedDocument{
		ID:                          structuredDoc.ID,
		Sequence:                    0,
		JWE:                         []byte(encryptedStructuredDoc),
		IndexedAttributeCollections: indexedAttributeCollections,
	}

	return encryptedDocument, nil
}

func generateEDVCompatibleID() (string, error) {
	randomBytes := make([]byte, 16)

	_, err := rand.Read(randomBytes)
	if err != nil {
		return "", err
	}

	base58EncodedUUID := base58.Encode(randomBytes)

	return base58EncodedUUID, nil
}

// StoreVerifiableCredential swagger:route POST /retrieve issuer retrieveCredentialReq
//
// Retrieves a stored credential.
//
// Responses:
//    default: genericError
//        200: emptyRes
func (o *Operation) retrieveCredentialHandler(rw http.ResponseWriter, req *http.Request) {
	id := req.URL.Query().Get("id")
	profile := req.URL.Query().Get("profile")

	if err := validateRequest(profile, id); err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, err.Error())

		return
	}

	docURLs, err := o.queryVault(profile, id)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusInternalServerError, err.Error())

		return
	}

	o.retrieveCredential(rw, profile, docURLs)
}

func (o *Operation) createIssuerProfile(pr *ProfileRequest) (*vcprofile.DataProfile, error) {
	var didID, publicKeyID string

	didID, publicKeyID, err := o.commonDID.CreateDID(pr.DIDKeyType, pr.SignatureType,
		pr.DID, pr.DIDPrivateKey, pr.DIDKeyID, crypto.AssertionMethod, pr.UNIRegistrar)
	if err != nil {
		return nil, err
	}

	created := time.Now().UTC()

	return &vcprofile.DataProfile{Name: pr.Name, URI: pr.URI, Created: &created, DID: didID,
		SignatureType: pr.SignatureType, SignatureRepresentation: pr.SignatureRepresentation, Creator: publicKeyID,
		DisableVCStatus: pr.DisableVCStatus, OverwriteIssuer: pr.OverwriteIssuer,
	}, nil
}

func validateProfileRequest(pr *ProfileRequest) error {
	if pr.Name == "" {
		return fmt.Errorf("missing profile name")
	}

	if pr.URI == "" {
		return fmt.Errorf("missing URI information")
	}

	if pr.SignatureType == "" {
		return fmt.Errorf("missing signature type")
	}

	_, err := url.Parse(pr.URI)
	if err != nil {
		return fmt.Errorf("invalid uri: %s", err.Error())
	}

	return nil
}

func validateRequest(profileName, vcID string) error {
	if profileName == "" {
		return fmt.Errorf("missing profile name")
	}

	if vcID == "" {
		return fmt.Errorf("missing verifiable credential ID")
	}

	return nil
}

// IssueCredential swagger:route POST /{id}/credentials/issueCredential issuer issueCredentialReq
//
// Issues a credential.
//
// Responses:
//    default: genericError
//        201: verifiableCredentialRes
// nolint: funlen
func (o *Operation) issueCredentialHandler(rw http.ResponseWriter, req *http.Request) {
	// get the issuer profile
	profileID := mux.Vars(req)[profileIDPathParam]

	profile, err := o.profileStore.GetProfile(profileID)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, fmt.Sprintf("invalid issuer profile - id=%s: err=%s",
			profileID, err.Error()))

		return
	}

	// get the request
	cred := IssueCredentialRequest{}

	err = json.NewDecoder(req.Body).Decode(&cred)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, fmt.Sprintf(invalidRequestErrMsg+": %s", err.Error()))

		return
	}

	// validate options
	if err = validateIssueCredOptions(cred.Opts); err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, err.Error())

		return
	}

	// validate the VC (ignore the proof)
	credential, _, err := verifiable.NewCredential(cred.Credential, verifiable.WithDisabledProofCheck())
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, fmt.Sprintf("failed to validate credential: %s", err.Error()))

		return
	}

	if !profile.DisableVCStatus {
		// set credential status
		credential.Status, err = o.vcStatusManager.CreateStatusID()
		if err != nil {
			commhttp.WriteErrorResponse(rw, http.StatusInternalServerError, fmt.Sprintf("failed to add credential status:"+
				" %s", err.Error()))

			return
		}

		credential.Context = append(credential.Context, cslstatus.Context)
	}

	// update context
	updateContext(credential, profile)

	// update credential issuer
	updateIssuer(credential, profile)

	// sign the credential
	signedVC, err := o.crypto.SignCredential(profile, credential, getIssuerSigningOpts(cred.Opts)...)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusInternalServerError, fmt.Sprintf("failed to sign credential:"+
			" %s", err.Error()))

		return
	}

	rw.WriteHeader(http.StatusCreated)
	commhttp.WriteResponse(rw, signedVC)
}

// nolint funlen
// composeAndIssueCredential swagger:route POST /{id}/credentials/composeAndIssueCredential issuer composeCredentialReq
//
// Composes and Issues a credential.
//
// Responses:
//    default: genericError
//        201: verifiableCredentialRes
func (o *Operation) composeAndIssueCredentialHandler(rw http.ResponseWriter, req *http.Request) {
	id := mux.Vars(req)[profileIDPathParam]

	profile, err := o.profileStore.GetProfile(id)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, fmt.Sprintf("invalid issuer profile: %s", err.Error()))

		return
	}

	// get the request
	composeCredReq := ComposeCredentialRequest{}

	err = json.NewDecoder(req.Body).Decode(&composeCredReq)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, fmt.Sprintf(invalidRequestErrMsg+": %s", err.Error()))

		return
	}

	// create the verifiable credential
	credential, err := buildCredential(&composeCredReq)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, fmt.Sprintf("failed to build credential:"+
			" %s", err.Error()))

		return
	}

	if !profile.DisableVCStatus {
		// set credential status
		credential.Status, err = o.vcStatusManager.CreateStatusID()
		if err != nil {
			commhttp.WriteErrorResponse(rw, http.StatusInternalServerError, fmt.Sprintf("failed to add credential status:"+
				" %s", err.Error()))

			return
		}

		credential.Context = append(credential.Context, cslstatus.Context)
	}

	// update context
	updateContext(credential, profile)

	// update credential issuer
	updateIssuer(credential, profile)

	// prepare signing options from request options
	opts, err := getComposeSigningOpts(&composeCredReq)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest, fmt.Sprintf("failed to prepare signing options:"+
			" %s", err.Error()))

		return
	}

	// sign the credential
	signedVC, err := o.crypto.SignCredential(profile, credential, opts...)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusInternalServerError, fmt.Sprintf("failed to sign credential:"+
			" %s", err.Error()))

		return
	}

	// response
	rw.WriteHeader(http.StatusCreated)
	commhttp.WriteResponse(rw, signedVC)
}

func buildCredential(composeCredReq *ComposeCredentialRequest) (*verifiable.Credential, error) {
	// create the verifiable credential
	credential := &verifiable.Credential{}

	// set credential data
	credential.Context = []string{"https://www.w3.org/2018/credentials/v1"}
	credential.Issued = composeCredReq.IssuanceDate
	credential.Expired = composeCredReq.ExpirationDate

	// set default type, if request doesn't contain the type
	credential.Types = []string{"VerifiableCredential"}
	if len(composeCredReq.Types) != 0 {
		credential.Types = composeCredReq.Types
	}

	// set subject
	credentialSubject := make(map[string]interface{})

	if composeCredReq.Claims != nil {
		err := json.Unmarshal(composeCredReq.Claims, &credentialSubject)
		if err != nil {
			return nil, err
		}
	}

	credentialSubject["id"] = composeCredReq.Subject
	credential.Subject = credentialSubject

	// set issuer
	credential.Issuer = verifiable.Issuer{
		ID: composeCredReq.Issuer,
	}

	// set terms of use
	termsOfUse, err := decodeTypedID(composeCredReq.TermsOfUse)
	if err != nil {
		return nil, err
	}

	credential.TermsOfUse = termsOfUse

	// set evidence
	if composeCredReq.Evidence != nil {
		evidence := make(map[string]interface{})

		err := json.Unmarshal(composeCredReq.Evidence, &evidence)
		if err != nil {
			return nil, err
		}

		credential.Evidence = evidence
	}

	return credential, nil
}

func decodeTypedID(typedIDBytes json.RawMessage) ([]verifiable.TypedID, error) {
	if len(typedIDBytes) == 0 {
		return nil, nil
	}

	var singleTypedID verifiable.TypedID

	err := json.Unmarshal(typedIDBytes, &singleTypedID)
	if err == nil {
		return []verifiable.TypedID{singleTypedID}, nil
	}

	var composedTypedID []verifiable.TypedID

	err = json.Unmarshal(typedIDBytes, &composedTypedID)
	if err == nil {
		return composedTypedID, nil
	}

	return nil, err
}

func getComposeSigningOpts(composeCredReq *ComposeCredentialRequest) ([]crypto.SigningOpts, error) {
	var proofFormatOptions struct {
		KeyID   string     `json:"kid,omitempty"`
		Purpose string     `json:"proofPurpose,omitempty"`
		Created *time.Time `json:"created,omitempty"`
	}

	if composeCredReq.ProofFormatOptions != nil {
		err := json.Unmarshal(composeCredReq.ProofFormatOptions, &proofFormatOptions)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare signing opts: %w", err)
		}
	}

	representation := "jws"
	if composeCredReq.ProofFormat != "" {
		representation = composeCredReq.ProofFormat
	}

	return []crypto.SigningOpts{
		crypto.WithPurpose(proofFormatOptions.Purpose),
		crypto.WithVerificationMethod(proofFormatOptions.KeyID),
		crypto.WithSigningRepresentation(representation),
		crypto.WithCreated(proofFormatOptions.Created),
	}, nil
}

func getIssuerSigningOpts(opts *IssueCredentialOptions) []crypto.SigningOpts {
	var signingOpts []crypto.SigningOpts

	if opts != nil {
		// verification method takes priority
		verificationMethod := opts.VerificationMethod

		if verificationMethod == "" {
			verificationMethod = opts.AssertionMethod
		}

		signingOpts = []crypto.SigningOpts{
			crypto.WithVerificationMethod(verificationMethod),
			crypto.WithPurpose(opts.ProofPurpose),
			crypto.WithCreated(opts.Created),
			crypto.WithChallenge(opts.Challenge),
			crypto.WithDomain(opts.Domain),
		}
	}

	return signingOpts
}

// GenerateKeypair swagger:route GET /kms/generatekeypair issuer req
//
// Generates a keypair, stores it in the KMS and returns the public key.
//
// Responses:
//    default: genericError
//        200: generateKeypairResp
func (o *Operation) generateKeypairHandler(rw http.ResponseWriter, req *http.Request) {
	keyID, signKey, err := o.createKey(kms.ED25519Type)
	if err != nil {
		commhttp.WriteErrorResponse(rw, http.StatusInternalServerError,
			fmt.Sprintf("failed to create key pair: %s", err.Error()))

		return
	}

	rw.WriteHeader(http.StatusOK)
	commhttp.WriteResponse(rw, &GenerateKeyPairResponse{
		PublicKey: base58.Encode(signKey),
		KeyID:     keyID,
	})
}

func (o *Operation) createKey(keyType kms.KeyType) (string, []byte, error) {
	keyID, _, err := o.kms.Create(keyType)
	if err != nil {
		return "", nil, err
	}

	pubKeyBytes, err := o.kms.ExportPubKeyBytes(keyID)
	if err != nil {
		return "", nil, err
	}

	return keyID, pubKeyBytes, nil
}

func (o *Operation) parseAndVerifyVC(vcBytes []byte) (*verifiable.Credential, error) {
	vc, _, err := verifiable.NewCredential(
		vcBytes,
		verifiable.WithPublicKeyFetcher(
			verifiable.NewDIDKeyResolver(o.vdri).PublicKeyFetcher(),
		),
	)

	if err != nil {
		return nil, err
	}

	return vc, nil
}

func (o *Operation) queryVault(vaultID, vcID string) ([]string, error) {
	vcIDMAC, err := o.macCrypto.ComputeMAC([]byte(vcID), o.macKeyHandle)
	if err != nil {
		return nil, err
	}

	vcIDIndexValueEncoded := base64.URLEncoding.EncodeToString(vcIDMAC)

	return o.edvClient.QueryVault(vaultID, &models.Query{
		Name:  o.vcIDIndexNameEncoded,
		Value: vcIDIndexValueEncoded,
	})
}

func (o *Operation) retrieveCredential(rw http.ResponseWriter, profileName string, docURLs []string) {
	var retrievedVC []byte

	switch len(docURLs) {
	case 0:
		commhttp.WriteErrorResponse(rw, http.StatusBadRequest,
			fmt.Sprintf(`no VC under profile "%s" was found with the given id`, profileName))
	case 1:
		docID := getDocIDFromURL(docURLs[0])

		var err error

		retrievedVC, err = o.retrieveVC(profileName, docID, "retrieving VC")
		if err != nil {
			commhttp.WriteErrorResponse(rw, http.StatusInternalServerError, err.Error())

			return
		}
	default:
		// Multiple VCs were found with the same id. This is technically possible under the right circumstances
		// when storing the same VC multiples times in a store provider that follows an "eventually consistent"
		// consistency model. If they are all the same, then just return the first one arbitrarily.
		// ToDo: If the multiple VCs with the same ID all are identical then delete the extras and leave only one.
		// https://github.com/trustbloc/edge-service/issues/262
		var err error

		var statusCode int

		retrievedVC, statusCode, err = o.verifyMultipleMatchingVCsAreIdentical(profileName, docURLs)
		if err != nil {
			commhttp.WriteErrorResponse(rw, statusCode, err.Error())

			return
		}
	}

	_, err := rw.Write(retrievedVC)
	if err != nil {
		log.Errorf("Failed to write response for document retrieval success: %s",
			err.Error())

		return
	}
}

func (o *Operation) verifyMultipleMatchingVCsAreIdentical(profileName string, docURLs []string) ([]byte, int, error) {
	var retrievedVCs [][]byte

	for _, docURL := range docURLs {
		docID := getDocIDFromURL(docURL)

		retrievedVC, err := o.retrieveVC(profileName, docID, "determining if the multiple VCs "+
			"matching the given ID are the same")
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}

		retrievedVCs = append(retrievedVCs, retrievedVC)
	}

	for i := 1; i < len(retrievedVCs); i++ {
		if !bytes.Equal(retrievedVCs[0], retrievedVCs[i]) {
			return nil, http.StatusConflict, errMultipleInconsistentVCsFoundForOneID
		}
	}

	return retrievedVCs[0], http.StatusOK, nil
}

func (o *Operation) retrieveVC(profileName, docID, contextErrText string) ([]byte, error) {
	document, err := o.edvClient.ReadDocument(profileName, docID)
	if err != nil {
		return nil, fmt.Errorf("failed to read document while %s: %s", contextErrText, err)
	}

	encryptedJWE, err := jose.Deserialize(string(document.JWE))
	if err != nil {
		return nil, err
	}

	decryptedDocBytes, err := o.jweDecrypter.Decrypt(encryptedJWE)
	if err != nil {
		return nil, fmt.Errorf("decrypting document failed while "+contextErrText+": %s", err)
	}

	decryptedDoc := models.StructuredDocument{}

	err = json.Unmarshal(decryptedDocBytes, &decryptedDoc)
	if err != nil {
		return nil, fmt.Errorf("decrypted structured document unmarshalling failed "+
			"while "+contextErrText+": %s", err)
	}

	retrievedVC, err := json.Marshal(decryptedDoc.Content["message"])
	if err != nil {
		return nil, fmt.Errorf("failed to marshall VC from decrypted document while "+
			contextErrText+": %s", err)
	}

	return retrievedVC, nil
}

// updateIssuer overrides credential issuer form profile if
// 'profile.OverwriteIssuer=true' or credential issuer is missing
// credential issue will always be DID
func updateIssuer(credential *verifiable.Credential, profile *vcprofile.DataProfile) {
	if profile.OverwriteIssuer || credential.Issuer.ID == "" {
		credential.Issuer = verifiable.Issuer{ID: profile.DID,
			CustomFields: verifiable.CustomFields{"name": profile.Name}}
	}
}

func updateContext(credential *verifiable.Credential, profile *vcprofile.DataProfile) {
	if profile.SignatureType == crypto.JSONWebSignature2020 {
		credential.Context = append(credential.Context, jsonWebSignature2020Context)
	}
}

func validateIssueCredOptions(options *IssueCredentialOptions) error {
	if options != nil {
		switch {
		case options.ProofPurpose != "":
			switch options.ProofPurpose {
			case assertionMethod, authentication, capabilityDelegation, capabilityInvocation:
			default:
				return fmt.Errorf("invalid proof option : %s", options.ProofPurpose)
			}
		case options.AssertionMethod != "":
			idSplit := strings.Split(options.AssertionMethod, "#")
			if len(idSplit) != 2 {
				return fmt.Errorf("invalid assertion method : %s", idSplit)
			}
		}
	}

	return nil
}

// Given an EDV document URL, returns just the document ID
func getDocIDFromURL(docURL string) string {
	splitBySlashes := strings.Split(docURL, `/`)
	docIDToRetrieve := splitBySlashes[len(splitBySlashes)-1]

	return docIDToRetrieve
}
