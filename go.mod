module github.com/soypat/tinyboot/cmd/picohub

go 1.25.0

require (
	github.com/a-h/templ v0.3.1020
	github.com/soypat/tinyboot v0.0.0-00010101000000-000000000000
	go.bug.st/serial v1.7.1
	go.etcd.io/bbolt v1.5.0
)

require golang.org/x/sys v0.45.0 // indirect

replace github.com/soypat/tinyboot => ../..
