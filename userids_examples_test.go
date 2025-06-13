package gomatrix_test

import (
	"fmt"

	"github.com/antinvestor/gomatrix"
)

func ExampleEncodeUserLocalpart() {
	localpart := gomatrix.EncodeUserLocalpart("Alph@Bet_50up")
	fmt.Println(localpart)
	// Output: _alph=40_bet__50up
}

func ExampleDecodeUserLocalpart() {
	localpart, err := gomatrix.DecodeUserLocalpart("_alph=40_bet__50up")
	if err != nil {
		panic(err)
	}
	fmt.Println(localpart)
	// Output: Alph@Bet_50up
}

func ExampleExtractUserLocalpart() {
	localpart, err := gomatrix.ExtractUserLocalpart("@alice:matrix.org")
	if err != nil {
		panic(err)
	}
	fmt.Println(localpart)
	// Output: alice
}
