all:
	GOOS=linux GOARCH=amd64 go build -o application application.go inspector.go blacklist.go region.go
	GOOS=linux GOARCH=amd64 go build -o populate populate.go region.go
	zip -r dufflebag.zip application populate .ebextensions/

clean:
	rm -f application populate dufflebag.zip
