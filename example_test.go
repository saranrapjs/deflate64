package deflate64_test

import (
	"archive/zip"
	"fmt"
	"io"
	"log"

	"github.com/saranrapjs/deflate64"
)

func ExampleDecompressor() {
	zr, err := zip.OpenReader("testdata/text.zip")
	if err != nil {
		log.Fatal(err)
	}
	defer zr.Close()
	zr.RegisterDecompressor(deflate64.ZipMethod, deflate64.Decompressor)

	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			log.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s: %d bytes, method %d\n", f.Name, len(data), f.Method)
	}
	// Output:
	// text.txt: 3200 bytes, method 9
}
