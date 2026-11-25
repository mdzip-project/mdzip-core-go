package mdzip_test

import (
	"errors"
	"fmt"

	mdzip "github.com/mdzip-project/mdzip-core-go"
)

func Example() {
	// Create an archive.
	a := mdzip.New()
	a.EditManifest().SetApplication("Example App", "1.0.0", "")
	a.WriteFile("index.md", []byte("# Field notes\n\n![map](images/map.png)\n"))
	a.WriteFile("images/map.png", []byte("…image bytes…"))
	data, err := a.Bytes() // or a.Save("notes.mdz")
	if err != nil {
		panic(err)
	}

	// Open it again and read the primary document.
	b, err := mdzip.OpenBytes(data)
	if err != nil {
		panic(err)
	}
	ep, err := b.EntryPoint()
	if errors.Is(err, mdzip.ErrEntryPointMissing) {
		fmt.Println("the manifest names a file that isn't there")
		return
	}
	text, _ := b.ReadText(ep)
	link, _ := b.Resolve(ep, "images/map.png")

	fmt.Println(ep, b.Manifest().Title(), b.Mode())
	fmt.Printf("%q\n", text[:15])
	fmt.Println(link.Kind == mdzip.LinkInternal, link.Entry)
	// Output:
	// index.md Field notes document
	// "# Field notes\n\n"
	// true images/map.png
}
