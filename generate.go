//go:generate rm -vf autogen/gentemplates/gen.go
//go:generate rm -vf autogen/genstatic/gen.go
//go:generate mkdir -p static
//go:generate go-bindata -pkg gentemplates -modtime 1509884496 -mode 436 -o autogen/gentemplates/gen.go ./templates/...
//go:generate gofmt -w autogen/gentemplates/gen.go
//go:generate go-bindata -pkg genstatic -o autogen/genstatic/gen.go ./static/...

package main

func main() {}
