// Package gomatrix implements the Matrix Client-Server API.
//
// Specification can be found at http://matrix.org/docs/spec/client_server/r0.2.0.html
package gomatrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Constants for the Matrix Client.
const (
	DefaultSyncTimeout     = 30000           // Default timeout for sync requests in milliseconds (30s)
	DefaultHTTPTimeout     = 5 * time.Second // Default HTTP request timeout
	StatusCodeOK           = 200             // Standard HTTP OK status code
	StatusCodeCreated      = 201             // Standard HTTP Created status code
	StatusCodeAccepted     = 202             // Standard HTTP Accepted status code
	StatusCode2xxOK        = 2               // First digit for 2xx HTTP status codes
	StatusCodeUnauthorized = 401             // HTTP Unauthorized status code
)

// Client represents a Matrix client.
type Client struct {
	HomeserverURL *url.URL     // The base homeserver URL
	Prefix        string       // The API prefix eg '/_matrix/client/r0'
	UserID        string       // The user ID of the client. Used for forming HTTP paths which use the client's user ID.
	AccessToken   string       // The access_token for the client.
	Client        *http.Client // The underlying HTTP client which will be used to make HTTP requests.
	Syncer        Syncer       // The thing which can process /sync responses
	Store         Storer       // The thing which can store rooms/tokens/ids

	// The ?user_id= query parameter for application services. This must be set *prior* to calling a method. If this is empty,
	// no user_id parameter will be sent.
	// See http://matrix.org/docs/spec/application_service/unstable.html#identity-assertion
	AppServiceUserID string

	syncingMutex sync.Mutex // protects syncingID
	syncingID    uint32     // Identifies the current Sync. Only one Sync can be active at any given time.
}

// HTTPError An HTTP Error response, which may wrap an underlying native Go Error.
type HTTPError struct {
	Contents     []byte
	WrappedError error
	Message      string
	Code         int
}

func (e HTTPError) Error() string {
	var wrappedErrMsg string
	if e.WrappedError != nil {
		wrappedErrMsg = e.WrappedError.Error()
	}
	return fmt.Sprintf("contents=%v msg=%s code=%d wrapped=%s", e.Contents, e.Message, e.Code, wrappedErrMsg)
}

// BuildURL builds a URL with the Client's homeserver/prefix set already.
func (cli *Client) BuildURL(urlPath ...string) string {
	ps := append([]string{cli.Prefix}, urlPath...)
	return cli.BuildBaseURL(ps...)
}

// BuildBaseURL builds a URL with the Client's homeserver set already. You must
// supply the prefix in the path.
func (cli *Client) BuildBaseURL(urlPath ...string) string {
	// copy the URL. Purposefully ignore error as the input is from a valid URL already
	hsURL, _ := url.Parse(cli.HomeserverURL.String())
	parts := []string{hsURL.Path}
	parts = append(parts, urlPath...)
	hsURL.Path = path.Join(parts...)
	// Manually add the trailing slash back to the end of the path if it's explicitly needed
	if strings.HasSuffix(urlPath[len(urlPath)-1], "/") {
		hsURL.Path += "/"
	}
	query := hsURL.Query()
	if cli.AppServiceUserID != "" {
		query.Set("user_id", cli.AppServiceUserID)
	}
	hsURL.RawQuery = query.Encode()
	return hsURL.String()
}

// BuildURLWithQuery builds a URL with query parameters in addition to the Client's homeserver/prefix set already.
func (cli *Client) BuildURLWithQuery(urlPath []string, urlQuery map[string]string) string {
	u, _ := url.Parse(cli.BuildURL(urlPath...))
	q := u.Query()
	for k, v := range urlQuery {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// SetCredentials sets the user ID and access token on this client instance.
func (cli *Client) SetCredentials(userID, accessToken string) {
	cli.AccessToken = accessToken
	cli.UserID = userID
}

// ClearCredentials removes the user ID and access token on this client instance.
func (cli *Client) ClearCredentials() {
	cli.AccessToken = ""
	cli.UserID = ""
}

// Sync starts syncing with the provided Homeserver. If Sync() is called twice then the first sync will be stopped and the
// error will be nil.
//
// This function will block until a fatal /sync error occurs, so it should almost always be started as a new goroutine.
// Fatal sync errors can be caused by:
//   - The failure to create a filter.
//   - Client.Syncer.OnFailedSync returning an error in response to a failed sync.
//   - Client.Syncer.ProcessResponse returning an error.
//
// If you wish to continue retrying in spite of these fatal errors, call Sync() again.
func (cli *Client) Sync() error {
	// Mark the client as syncing.
	// We will keep syncing until the syncing state changes. Either because
	// Sync is called or StopSync is called.
	syncingID := cli.incrementSyncingID()
	nextBatch := cli.Store.LoadNextBatch(cli.UserID)
	filterID := cli.Store.LoadFilterID(cli.UserID)
	if filterID == "" {
		filterJSON := cli.Syncer.GetFilterJSON(cli.UserID)
		resFilter, err := cli.CreateFilter(filterJSON)
		if err != nil {
			return err
		}
		filterID = resFilter.FilterID
		cli.Store.SaveFilterID(cli.UserID, filterID)
	}

	for {
		resSync, err := cli.SyncRequest(DefaultSyncTimeout, nextBatch, filterID, false, "")
		if err != nil {
			duration, err2 := cli.Syncer.OnFailedSync(resSync, err)
			if err2 != nil {
				return err2
			}
			time.Sleep(duration)
			continue
		}

		// Check that the syncing state hasn't changed
		// Either because we've stopped syncing or another sync has been started.
		// We discard the response from our sync.
		if cli.getSyncingID() != syncingID {
			return nil
		}

		// Save the token now *before* processing it. This means it's possible
		// to not process some events, but it means that we won't get constantly stuck processing
		// a malformed/buggy event which keeps making us panic.
		cli.Store.SaveNextBatch(cli.UserID, resSync.NextBatch)
		if err = cli.Syncer.ProcessResponse(resSync, nextBatch); err != nil {
			return err
		}

		nextBatch = resSync.NextBatch
	}
}

func (cli *Client) incrementSyncingID() uint32 {
	cli.syncingMutex.Lock()
	defer cli.syncingMutex.Unlock()
	cli.syncingID++
	return cli.syncingID
}

func (cli *Client) getSyncingID() uint32 {
	cli.syncingMutex.Lock()
	defer cli.syncingMutex.Unlock()
	return cli.syncingID
}

// StopSync stops the ongoing sync started by Sync.
func (cli *Client) StopSync() {
	// Advance the syncing state so that any running Syncs will terminate.
	cli.incrementSyncingID()
}

// MakeRequest makes a JSON HTTP request to the given URL.
// The response body will be stream decoded into an interface. This will automatically stop if the response
// body is nil.
//
// Returns an error if the response is not 2xx along with the HTTP body bytes if it got that far. This error is
// an HTTPError which includes the returned HTTP status code, byte contents of the response body and possibly a
// RespError as the WrappedError, if the HTTP body could be decoded as a RespError.
func (cli *Client) MakeRequest(method string, httpURL string, reqBody interface{}, resBody interface{}) error {
	// Create a context with timeout for HTTP requests
	ctx, cancel := context.WithTimeout(context.Background(), DefaultHTTPTimeout)
	defer cancel()

	var req *http.Request
	var err error
	if reqBody != nil {
		buf := new(bytes.Buffer)
		if encErr := json.NewEncoder(buf).Encode(reqBody); encErr != nil {
			return encErr
		}
		req, err = http.NewRequestWithContext(ctx, method, httpURL, buf)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, httpURL, nil)
	}

	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	if cli.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+cli.AccessToken)
	}

	res, err := cli.Client.Do(req)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return err
	}
	if res.StatusCode/100 != StatusCode2xxOK { // not 2xx
		contents, readErr := io.ReadAll(res.Body)
		if readErr != nil {
			return readErr
		}

		var wrap error
		var respErr RespError
		if _ = json.Unmarshal(contents, &respErr); respErr.ErrCode != "" {
			wrap = respErr
		}

		// If we failed to decode as RespError, don't just drop the HTTP body, include it in the
		// HTTP error.
		return HTTPError{
			Contents:     contents,
			Code:         res.StatusCode,
			Message:      "Failed to " + method + " JSON to " + req.URL.Path,
			WrappedError: wrap,
		}
	}

	if resBody != nil && res.Body != nil {
		return json.NewDecoder(res.Body).Decode(&resBody)
	}

	return nil
}

// CreateFilter makes an HTTP request according to http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-user-userid-filter
func (cli *Client) CreateFilter(filter json.RawMessage) (*RespCreateFilter, error) {
	urlPath := cli.BuildURL("user", cli.UserID, "filter")
	var resp RespCreateFilter
	err := cli.MakeRequest("POST", urlPath, &filter, &resp)
	return &resp, err
}

// SyncRequest makes an HTTP request according to http://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-r0-sync
func (cli *Client) SyncRequest(
	timeout int,
	since, filterID string,
	fullState bool,
	setPresence string,
) (*RespSync, error) {
	query := map[string]string{
		"timeout": strconv.Itoa(timeout),
	}
	if since != "" {
		query["since"] = since
	}
	if filterID != "" {
		query["filter"] = filterID
	}
	if setPresence != "" {
		query["set_presence"] = setPresence
	}
	if fullState {
		query["full_state"] = "true"
	}
	urlPath := cli.BuildURLWithQuery([]string{"sync"}, query)
	var resp RespSync
	err := cli.MakeRequest("GET", urlPath, nil, &resp)
	return &resp, err
}

func (cli *Client) register(u string, req *ReqRegister) (*RespRegister, *RespUserInteractive, error) {
	var resp RespRegister
	err := cli.MakeRequest("POST", u, req, &resp)
	if err != nil {
		var httpErr HTTPError
		if !errors.As(err, &httpErr) { // network error
			return &resp, nil, err
		}
		if httpErr.Code == StatusCodeUnauthorized {
			// body should be RespUserInteractive, if it isn't, fail with the error
			var uiaResp RespUserInteractive
			err = json.Unmarshal(httpErr.Contents, &uiaResp)
			return &resp, &uiaResp, err
		}
		return &resp, nil, err
	}

	return &resp, nil, nil
}

// Register makes an HTTP request according to http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-register
//
// Registers with kind=user. For kind=guest, see RegisterGuest.
func (cli *Client) Register(req *ReqRegister) (*RespRegister, *RespUserInteractive, error) {
	u := cli.BuildURL("register")
	return cli.register(u, req)
}

// RegisterGuest makes an HTTP request according to http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-register
// with kind=guest.
//
// For kind=user, see Register.
func (cli *Client) RegisterGuest(req *ReqRegister) (*RespRegister, *RespUserInteractive, error) {
	query := map[string]string{
		"kind": "guest",
	}
	u := cli.BuildURLWithQuery([]string{"register"}, query)
	return cli.register(u, req)
}

// RegisterDummy performs m.login.dummy registration according to https://matrix.org/docs/spec/client_server/r0.2.0.html#dummy-auth
//
// Only a username and password need to be provided on the ReqRegister struct. Most local/developer homeservers will allow registration
// this way. If the homeserver does not, an error is returned.
//
// This does not set credentials on the client instance. See SetCredentials() instead.
//
//		res, err := cli.RegisterDummy(&gomatrix.ReqRegister{
//			Username: "alice",
//			Password: "wonderland",
//		})
//	 if err != nil {
//			panic(err)
//		}
//		token := res.AccessToken
func (cli *Client) RegisterDummy(req *ReqRegister) (*RespRegister, error) {
	res, uia, err := cli.Register(req)
	if err != nil && uia == nil {
		return nil, err
	}
	if uia != nil && uia.HasSingleStageFlow("m.login.dummy") {
		req.Auth = struct {
			Type    string `json:"type"`
			Session string `json:"session,omitempty"`
		}{"m.login.dummy", uia.Session}
		res, _, err = cli.Register(req)
		if err != nil {
			return nil, err
		}
	}
	if res == nil {
		return nil, errors.New("registration failed: does this server support m.login.dummy?")
	}
	return res, nil
}

// Login a user to the homeserver according to http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-login
// This does not set credentials on this client instance. See SetCredentials() instead.
func (cli *Client) Login(req *ReqLogin) (*RespLogin, error) {
	urlPath := cli.BuildURL("login")
	var resp RespLogin
	err := cli.MakeRequest("POST", urlPath, req, &resp)
	return &resp, err
}

// Logout the current user. See http://matrix.org/docs/spec/client_server/r0.6.0.html#post-matrix-client-r0-logout
// This does not clear the credentials from the client instance. See ClearCredentials() instead.
func (cli *Client) Logout() (*RespLogout, error) {
	urlPath := cli.BuildURL("logout")
	var resp RespLogout
	err := cli.MakeRequest("POST", urlPath, nil, &resp)
	return &resp, err
}

// LogoutAll logs the current user out on all devices. See https://matrix.org/docs/spec/client_server/r0.6.0#post-matrix-client-r0-logout-all
// This does not clear the credentials from the client instance. See ClearCredentails() instead.
func (cli *Client) LogoutAll() (*RespLogoutAll, error) {
	urlPath := cli.BuildURL("logout/all")
	var resp RespLogoutAll
	err := cli.MakeRequest("POST", urlPath, nil, &resp)
	return &resp, err
}

// Versions returns the list of supported Matrix versions on this homeserver. See http://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-versions
func (cli *Client) Versions() (*RespVersions, error) {
	urlPath := cli.BuildBaseURL("_matrix", "client", "versions")
	var resp RespVersions
	err := cli.MakeRequest("GET", urlPath, nil, &resp)
	return &resp, err
}

// PublicRooms returns the list of public rooms on target server. See https://matrix.org/docs/spec/client_server/r0.6.0#get-matrix-client-unstable-publicrooms
func (cli *Client) PublicRooms(limit int, since string, server string) (*RespPublicRooms, error) {
	args := map[string]string{}

	if limit != 0 {
		args["limit"] = strconv.Itoa(limit)
	}
	if since != "" {
		args["since"] = since
	}
	if server != "" {
		args["server"] = server
	}

	urlPath := cli.BuildURLWithQuery([]string{"publicRooms"}, args)
	var resp RespPublicRooms
	err := cli.MakeRequest("GET", urlPath, nil, &resp)
	return &resp, err
}

// PublicRoomsFiltered returns a subset of PublicRooms filtered server side.
// See https://matrix.org/docs/spec/client_server/r0.6.0#post-matrix-client-unstable-publicrooms
func (cli *Client) PublicRoomsFiltered(
	limit int,
	since string,
	server string,
	filter string,
) (*RespPublicRooms, error) {
	content := map[string]string{}

	if limit != 0 {
		content["limit"] = strconv.Itoa(limit)
	}
	if since != "" {
		content["since"] = since
	}
	if filter != "" {
		content["filter"] = filter
	}

	var urlPath string
	if server == "" {
		urlPath = cli.BuildURL("publicRooms")
	} else {
		urlPath = cli.BuildURLWithQuery([]string{"publicRooms"}, map[string]string{
			"server": server,
		})
	}

	var resp RespPublicRooms
	err := cli.MakeRequest("POST", urlPath, content, &resp)
	return &resp, err
}

// JoinRoom joins the client to a room ID or alias. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-join-roomidoralias
//
// If serverName is specified, this will be added as a query param to instruct the homeserver to join via that server. If content is specified, it will
// be JSON encoded and used as the request body.
func (cli *Client) JoinRoom(roomIDorAlias, serverName string, content interface{}) (*RespJoinRoom, error) {
	var urlPath string
	if serverName != "" {
		urlPath = cli.BuildURLWithQuery([]string{"join", roomIDorAlias}, map[string]string{
			"server_name": serverName,
		})
	} else {
		urlPath = cli.BuildURL("join", roomIDorAlias)
	}
	var resp RespJoinRoom
	err := cli.MakeRequest("POST", urlPath, content, &resp)
	return &resp, err
}

// GetDisplayName returns the display name of the user from the specified MXID. See https://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-r0-profile-userid-displayname
func (cli *Client) GetDisplayName(mxid string) (*RespUserDisplayName, error) {
	urlPath := cli.BuildURL("profile", mxid, "displayname")
	var resp RespUserDisplayName
	err := cli.MakeRequest("GET", urlPath, nil, &resp)
	return &resp, err
}

// GetOwnDisplayName returns the user's display name. See https://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-r0-profile-userid-displayname
func (cli *Client) GetOwnDisplayName() (*RespUserDisplayName, error) {
	urlPath := cli.BuildURL("profile", cli.UserID, "displayname")
	var resp RespUserDisplayName
	err := cli.MakeRequest("GET", urlPath, nil, &resp)
	return &resp, err
}

// SetDisplayName sets the user's profile display name. See http://matrix.org/docs/spec/client_server/r0.2.0.html#put-matrix-client-r0-profile-userid-displayname
func (cli *Client) SetDisplayName(displayName string) error {
	urlPath := cli.BuildURL("profile", cli.UserID, "displayname")
	s := struct {
		DisplayName string `json:"displayname"`
	}{displayName}
	return cli.MakeRequest("PUT", urlPath, &s, nil)
}

// GetAvatarURL gets the user's avatar URL. See http://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-r0-profile-userid-avatar-url
func (cli *Client) GetAvatarURL() (string, error) {
	urlPath := cli.BuildURL("profile", cli.UserID, "avatar_url")
	s := struct {
		AvatarURL string `json:"avatar_url"`
	}{}

	err := cli.MakeRequest("GET", urlPath, nil, &s)
	if err != nil {
		return "", err
	}

	return s.AvatarURL, nil
}

// SetAvatarURL sets the user's avatar URL. See http://matrix.org/docs/spec/client_server/r0.2.0.html#put-matrix-client-r0-profile-userid-avatar-url
func (cli *Client) SetAvatarURL(url string) error {
	urlPath := cli.BuildURL("profile", cli.UserID, "avatar_url")
	s := struct {
		AvatarURL string `json:"avatar_url"`
	}{url}
	err := cli.MakeRequest("PUT", urlPath, &s, nil)
	if err != nil {
		return err
	}

	return nil
}

// GetStatus returns the status of the user from the specified MXID. See https://matrix.org/docs/spec/client_server/r0.6.0#get-matrix-client-r0-presence-userid-status
func (cli *Client) GetStatus(mxid string) (*RespUserStatus, error) {
	urlPath := cli.BuildURL("presence", mxid, "status")
	var resp RespUserStatus
	err := cli.MakeRequest("GET", urlPath, nil, &resp)
	return &resp, err
}

// GetOwnStatus returns the user's status. See https://matrix.org/docs/spec/client_server/r0.6.0#get-matrix-client-r0-presence-userid-status
func (cli *Client) GetOwnStatus() (*RespUserStatus, error) {
	return cli.GetStatus(cli.UserID)
}

// SetStatus sets the user's status. See https://matrix.org/docs/spec/client_server/r0.6.0#put-matrix-client-r0-presence-userid-status
func (cli *Client) SetStatus(presence, status string) error {
	urlPath := cli.BuildURL("presence", cli.UserID, "status")
	s := struct {
		Presence  string `json:"presence"`
		StatusMsg string `json:"status_msg"`
	}{presence, status}
	return cli.MakeRequest("PUT", urlPath, &s, nil)
}

// SendMessageEvent sends a message event into a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#put-matrix-client-r0-rooms-roomid-send-eventtype-txnid
// contentJSON should be a pointer to something that can be encoded as JSON using json.Marshal.
func (cli *Client) SendMessageEvent(
	roomID string,
	eventType string,
	contentJSON interface{},
) (*RespSendEvent, error) {
	txnID := txnID()
	urlPath := cli.BuildURL("rooms", roomID, "send", eventType, txnID)
	var resp RespSendEvent
	err := cli.MakeRequest("PUT", urlPath, contentJSON, &resp)
	return &resp, err
}

// SendStateEvent sends a state event into a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#put-matrix-client-r0-rooms-roomid-state-eventtype-statekey
// contentJSON should be a pointer to something that can be encoded as JSON using json.Marshal.
func (cli *Client) SendStateEvent(
	roomID, eventType, stateKey string,
	contentJSON interface{},
) (*RespSendEvent, error) {
	urlPath := cli.BuildURL("rooms", roomID, "state", eventType, stateKey)
	var resp RespSendEvent
	err := cli.MakeRequest("PUT", urlPath, contentJSON, &resp)
	return &resp, err
}

// SendText sends an m.room.message event into the given room with a msgtype of m.text
// See http://matrix.org/docs/spec/client_server/r0.2.0.html#m-text
func (cli *Client) SendText(roomID, text string) (*RespSendEvent, error) {
	return cli.SendMessageEvent(roomID, "m.room.message",
		TextMessage{MsgType: "m.text", Body: text})
}

// SendFormattedText sends an m.room.message event into the given room with a msgtype of m.text, supports a subset of HTML for formatting.
// See https://matrix.org/docs/spec/client_server/r0.6.0#m-text
func (cli *Client) SendFormattedText(roomID, text, formattedText string) (*RespSendEvent, error) {
	return cli.SendMessageEvent(roomID, "m.room.message",
		TextMessage{MsgType: "m.text", Body: text, FormattedBody: formattedText, Format: "org.matrix.custom.html"})
}

// SendImage sends an m.room.message event into the given room with a msgtype of m.image
// See https://matrix.org/docs/spec/client_server/r0.2.0.html#m-image
func (cli *Client) SendImage(roomID, body, url string) (*RespSendEvent, error) {
	return cli.SendMessageEvent(roomID, "m.room.message",
		ImageMessage{
			MsgType: "m.image",
			Body:    body,
			URL:     url,
		})
}

// SendVideo sends an m.room.message event into the given room with a msgtype of m.video
// See https://matrix.org/docs/spec/client_server/r0.2.0.html#m-video
func (cli *Client) SendVideo(roomID, body, url string) (*RespSendEvent, error) {
	return cli.SendMessageEvent(roomID, "m.room.message",
		VideoMessage{
			MsgType: "m.video",
			Body:    body,
			URL:     url,
		})
}

// SendNotice sends an m.room.message event into the given room with a msgtype of m.notice
// See http://matrix.org/docs/spec/client_server/r0.2.0.html#m-notice
func (cli *Client) SendNotice(roomID, text string) (*RespSendEvent, error) {
	return cli.SendMessageEvent(roomID, "m.room.message",
		TextMessage{MsgType: "m.notice", Body: text})
}

// RedactEvent redacts the given event. See http://matrix.org/docs/spec/client_server/r0.2.0.html#put-matrix-client-r0-rooms-roomid-redact-eventid-txnid
func (cli *Client) RedactEvent(roomID, eventID string, req *ReqRedact) (*RespSendEvent, error) {
	txnID := txnID()
	urlPath := cli.BuildURL("rooms", roomID, "redact", eventID, txnID)
	var resp RespSendEvent
	err := cli.MakeRequest("PUT", urlPath, req, &resp)
	return &resp, err
}

// MarkRead marks eventID in roomID as read, signifying the event, and all before it have been read. See https://matrix.org/docs/spec/client_server/r0.6.0#post-matrix-client-r0-rooms-roomid-receipt-receipttype-eventid
func (cli *Client) MarkRead(roomID, eventID string) error {
	urlPath := cli.BuildURL("rooms", roomID, "receipt", "m.read", eventID)
	return cli.MakeRequest("POST", urlPath, nil, nil)
}

// CreateRoom creates a new Matrix room. See https://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-createroom
//
//	resp, err := cli.CreateRoom(&gomatrix.ReqCreateRoom{
//		Preset: "public_chat",
//	})
//	fmt.Println("Room:", resp.RoomID)
func (cli *Client) CreateRoom(req *ReqCreateRoom) (*RespCreateRoom, error) {
	urlPath := cli.BuildURL("createRoom")
	var resp RespCreateRoom
	err := cli.MakeRequest("POST", urlPath, req, &resp)
	return &resp, err
}

// LeaveRoom leaves the given room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-rooms-roomid-leave
func (cli *Client) LeaveRoom(roomID string) (*RespLeaveRoom, error) {
	u := cli.BuildURL("rooms", roomID, "leave")
	var resp RespLeaveRoom
	err := cli.MakeRequest("POST", u, struct{}{}, &resp)
	return &resp, err
}

// ForgetRoom forgets a room entirely. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-rooms-roomid-forget
func (cli *Client) ForgetRoom(roomID string) (*RespForgetRoom, error) {
	u := cli.BuildURL("rooms", roomID, "forget")
	var resp RespForgetRoom
	err := cli.MakeRequest("POST", u, struct{}{}, &resp)
	return &resp, err
}

// InviteUser invites a user to a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-rooms-roomid-invite
func (cli *Client) InviteUser(roomID string, req *ReqInviteUser) (*RespInviteUser, error) {
	u := cli.BuildURL("rooms", roomID, "invite")
	var resp RespInviteUser
	err := cli.MakeRequest("POST", u, req, &resp)
	return &resp, err
}

// InviteUserByThirdParty invites a third-party identifier to a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#invite-by-third-party-id-endpoint
func (cli *Client) InviteUserByThirdParty(roomID string, req *ReqInvite3PID) (*RespInviteUser, error) {
	u := cli.BuildURL("rooms", roomID, "invite")
	var resp RespInviteUser
	err := cli.MakeRequest("POST", u, req, &resp)
	return &resp, err
}

// KickUser kicks a user from a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-rooms-roomid-kick
func (cli *Client) KickUser(roomID string, req *ReqKickUser) (*RespKickUser, error) {
	u := cli.BuildURL("rooms", roomID, "kick")
	var resp RespKickUser
	err := cli.MakeRequest("POST", u, req, &resp)
	return &resp, err
}

// BanUser bans a user from a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-rooms-roomid-ban
func (cli *Client) BanUser(roomID string, req *ReqBanUser) (*RespBanUser, error) {
	u := cli.BuildURL("rooms", roomID, "ban")
	var resp RespBanUser
	err := cli.MakeRequest("POST", u, req, &resp)
	return &resp, err
}

// UnbanUser unbans a user from a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-rooms-roomid-unban
func (cli *Client) UnbanUser(roomID string, req *ReqUnbanUser) (*RespUnbanUser, error) {
	u := cli.BuildURL("rooms", roomID, "unban")
	var resp RespUnbanUser
	err := cli.MakeRequest("POST", u, req, &resp)
	return &resp, err
}

// UserTyping sets the typing status of the user. See https://matrix.org/docs/spec/client_server/r0.2.0.html#put-matrix-client-r0-rooms-roomid-typing-userid
func (cli *Client) UserTyping(roomID string, typing bool, timeout int64) (*RespTyping, error) {
	req := ReqTyping{Typing: typing, Timeout: timeout}
	u := cli.BuildURL("rooms", roomID, "typing", cli.UserID)
	var resp RespTyping
	err := cli.MakeRequest("PUT", u, req, &resp)
	return &resp, err
}

// StateEvent gets a single state event in a room. It will attempt to JSON unmarshal into the given "outContent" struct with
// the HTTP response body, or return an error.
// See http://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-r0-rooms-roomid-state-eventtype-statekey
func (cli *Client) StateEvent(roomID, eventType, stateKey string, outContent interface{}) error {
	u := cli.BuildURL("rooms", roomID, "state", eventType, stateKey)
	return cli.MakeRequest("GET", u, nil, outContent)
}

// UploadLink uploads an HTTP URL and then returns an MXC URI.
func (cli *Client) UploadLink(link string) (*RespMediaUpload, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}

	res, err := cli.Client.Do(req)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return nil, err
	}

	return cli.UploadToContentRepo(res.Body, res.Header.Get("Content-Type"), res.ContentLength)
}

// UploadToContentRepo uploads the given bytes to the content repository and returns an MXC URI.
// See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-media-r0-upload
func (cli *Client) UploadToContentRepo(
	content io.Reader,
	contentType string,
	contentLength int64,
) (*RespMediaUpload, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cli.BuildBaseURL("_matrix/media/r0/upload"), content)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+cli.AccessToken)

	req.ContentLength = contentLength

	res, err := cli.Client.Do(req)
	if res != nil {
		defer res.Body.Close()
	}

	if err != nil {
		return nil, err
	}

	if res.StatusCode != StatusCodeOK {
		contents, readErr := io.ReadAll(res.Body)
		if readErr != nil {
			return nil, HTTPError{
				Message: "Upload request failed - Failed to read response body: " + readErr.Error(),
				Code:    res.StatusCode,
			}
		}
		return nil, HTTPError{
			Contents: contents,
			Message:  "Upload request failed: " + string(contents),
			Code:     res.StatusCode,
		}
	}

	var m RespMediaUpload
	if decodeErr := json.NewDecoder(res.Body).Decode(&m); decodeErr != nil {
		return nil, decodeErr
	}

	return &m, nil
}

// JoinedMembers returns a map of joined room members. See TODO-SPEC. https://github.com/matrix-org/synapse/pull/1680
//
// In general, usage of this API is discouraged in favour of /sync, as calling this API can race with incoming membership changes.
// This API is primarily designed for application services which may want to efficiently look up joined members in a room.
func (cli *Client) JoinedMembers(roomID string) (*RespJoinedMembers, error) {
	u := cli.BuildURL("rooms", roomID, "joined_members")
	var resp RespJoinedMembers
	err := cli.MakeRequest("GET", u, nil, &resp)
	return &resp, err
}

// JoinedRooms returns a list of rooms which the client is joined to. See TODO-SPEC. https://github.com/matrix-org/synapse/pull/1680
//
// In general, usage of this API is discouraged in favour of /sync, as calling this API can race with incoming membership changes.
// This API is primarily designed for application services which may want to efficiently look up joined rooms.
func (cli *Client) JoinedRooms() (*RespJoinedRooms, error) {
	u := cli.BuildURL("joined_rooms")
	var resp RespJoinedRooms
	err := cli.MakeRequest("GET", u, nil, &resp)
	return &resp, err
}

// Messages returns a list of message and state events for a room. It uses
// pagination query parameters to paginate history in the room.
// See https://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-r0-rooms-roomid-messages
func (cli *Client) Messages(roomID, from, to string, dir rune, limit int) (*RespMessages, error) {
	query := map[string]string{
		"from": from,
		"dir":  string(dir),
	}
	if to != "" {
		query["to"] = to
	}
	if limit != 0 {
		query["limit"] = strconv.Itoa(limit)
	}

	urlPath := cli.BuildURLWithQuery([]string{"rooms", roomID, "messages"}, query)
	var resp RespMessages
	err := cli.MakeRequest("GET", urlPath, nil, &resp)
	return &resp, err
}

// TurnServer returns turn server details and credentials for the client to use when initiating calls.
// See http://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-r0-voip-turnserver
func (cli *Client) TurnServer() (*RespTurnServer, error) {
	urlPath := cli.BuildURL("voip", "turnServer")
	var resp RespTurnServer
	err := cli.MakeRequest("GET", urlPath, nil, &resp)
	return &resp, err
}

func txnID() string {
	return "go" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// NewClient creates a new Matrix Client ready for syncing.
func NewClient(homeserverURL, userID, accessToken string) (*Client, error) {
	hsURL, err := url.Parse(homeserverURL)
	if err != nil {
		return nil, err
	}
	// By default, use an in-memory store which will never save filter ids / next batch tokens to disk.
	// The client will work with this storer: it just won't remember across restarts.
	// In practice, a database backend should be used.
	store := NewInMemoryStore()
	cli := Client{
		AccessToken:   accessToken,
		HomeserverURL: hsURL,
		UserID:        userID,
		Prefix:        "/_matrix/client/r0",
		Syncer:        NewDefaultSyncer(userID, store),
		Store:         store,
	}
	// By default, use the default HTTP client.
	cli.Client = http.DefaultClient

	return &cli, nil
}
