// go.mod declares the ccsbx module. It exists to keep the build to the standard library:
// there are no requires, and `make vet` fails if a go.sum appears.

module github.com/landovsky/cc-sandbox

go 1.27
