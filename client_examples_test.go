package gomatrix_test

import (
	"fmt"
	"net/http"

	"github.com/antinvestor/gomatrix"
)

// Example_sync demonstrates how to use the Sync API.
// The actual code is not run during tests.
//
//nolint:testableexamples // This example would make network calls if executed
func Example_sync() {
	cli, _ := gomatrix.NewClient("https://matrix.org", "@example:matrix.org", "MDAefhiuwehfuiwe")
	cli.Store.SaveFilterID("@example:matrix.org", "2")                // Optional: if you know it already
	cli.Store.SaveNextBatch("@example:matrix.org", "111_222_333_444") // Optional: if you know it already
	syncer := cli.Syncer.(*gomatrix.DefaultSyncer)
	syncer.OnEventType("m.room.message", func(ev *gomatrix.Event) {
		fmt.Println("Message: ", ev)
	})

	// Blocking version
	if err := cli.Sync(); err != nil {
		fmt.Println("Sync() returned ", err)
	}

	// Non-blocking version
	go func() {
		for {
			if err := cli.Sync(); err != nil {
				fmt.Println("Sync() returned ", err)
			}
			// Optional: Wait a period of time before trying to sync again.
		}
	}()
}

//nolint:testableexamples // This example would make network calls if executed
func Example_customInterfaces() {
	// Custom interfaces must be set prior to calling functions on the client.
	cli, _ := gomatrix.NewClient("https://matrix.org", "@example:matrix.org", "MDAefhiuwehfuiwe")

	// anything which implements the Storer interface
	customStore := gomatrix.NewInMemoryStore()
	cli.Store = customStore

	// anything which implements the Syncer interface
	customSyncer := gomatrix.NewDefaultSyncer("@example:matrix.org", customStore)
	cli.Syncer = customSyncer

	// any http.Client
	cli.Client = http.DefaultClient

	// Once you call a function, you can't safely change the interfaces.
	_, _ = cli.SendText("!foo:bar", "Down the rabbit hole")
}

func ExampleClient_BuildURLWithQuery() {
	cli, _ := gomatrix.NewClient("https://matrix.org", "@example:matrix.org", "abcdef123456")
	out := cli.BuildURLWithQuery([]string{"sync"}, map[string]string{
		"filter_id": "5",
	})
	fmt.Println(out)
	// Output: https://matrix.org/_matrix/client/r0/sync?filter_id=5
}

func ExampleClient_BuildURL() {
	userID := "@example:matrix.org"
	cli, _ := gomatrix.NewClient("https://matrix.org", userID, "abcdef123456")
	out := cli.BuildURL("user", userID, "filter")
	fmt.Println(out)
	// Output: https://matrix.org/_matrix/client/r0/user/@example:matrix.org/filter
}

func ExampleClient_BuildBaseURL() {
	userID := "@example:matrix.org"
	cli, _ := gomatrix.NewClient("https://matrix.org", userID, "abcdef123456")
	out := cli.BuildBaseURL("_matrix", "client", "r0", "directory", "room", "#matrix:matrix.org")
	fmt.Println(out)
	// Output: https://matrix.org/_matrix/client/r0/directory/room/%23matrix:matrix.org
}

// Retrieve the content of a m.room.name state event.
// The actual code is not run during tests.
//
//nolint:testableexamples // This example would make network calls if executed
func ExampleClient_StateEvent() {
	content := struct {
		Name string `json:"name"`
	}{}
	cli, _ := gomatrix.NewClient("https://matrix.org", "@example:matrix.org", "abcdef123456")
	if err := cli.StateEvent("!foo:bar", "m.room.name", "", &content); err != nil {
		panic(err)
	}
	fmt.Println("Room name:", content.Name) // This line won't execute in tests
}

// Join a room by ID.
// The actual code is not run during tests.
//
//nolint:testableexamples // This example would make network calls if executed
func ExampleClient_JoinRoom_id() {
	cli, _ := gomatrix.NewClient("http://localhost:8008", "@example:localhost", "abcdef123456")
	if _, err := cli.JoinRoom("!uOILRrqxnsYgQdUzar:localhost", "", nil); err != nil {
		panic(err)
	}
	fmt.Println("Room joined successfully") // This line won't execute in tests
}

// Join a room by alias.
// The actual code is not run during tests.
//
//nolint:testableexamples // This example would make network calls if executed
func ExampleClient_JoinRoom_alias() {
	cli, _ := gomatrix.NewClient("http://localhost:8008", "@example:localhost", "abcdef123456")
	resp, err := cli.JoinRoom("#test:localhost", "", nil)
	if err != nil {
		panic(err)
	}
	// Use room ID for something.
	fmt.Println("Joined room ID:", resp.RoomID) // This line won't execute in tests
}

// Login to a local homeserver and set the user ID and access token on success.
// The actual code is not run during tests.
//
//nolint:testableexamples // This example would make network calls if executed
func ExampleClient_Login() {
	cli, _ := gomatrix.NewClient("http://localhost:8008", "", "")
	resp, err := cli.Login(&gomatrix.ReqLogin{
		Type:     "m.login.password",
		User:     "alice",
		Password: "wonderland",
	})
	if err != nil {
		panic(err)
	}
	cli.SetCredentials(resp.UserID, resp.AccessToken)
	fmt.Println("Logged in as:", resp.UserID) // This line won't execute in tests
}
