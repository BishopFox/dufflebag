GOARGS := GOOS=linux GOARCH=amd64 go build -o

.PHONY: fetchdependencies
fetchdependencies:
	@echo "Generating temporary module."
	@go mod init github.com/BishopFox/dufflebag
	@go mod tidy

.PHONY: all
all: fetchdependencies application populate dufflebag.zip

application: application.go inspector.go blacklist.go region.go
	@echo "Building application binary."
	${GOARGS} $@ $^

populate: populate.go region.go
	@echo "Building populate binary."
	@${GOARGS} $@ $^

dufflebag.zip: application populate .ebextensions/
	@echo "Compressing dufflebag.zip."
	@zip -r $@ $^

.PHONY: clean
clean:
	@rm -f application populate dufflebag.zip
	@rm -f go.mod go.sum
