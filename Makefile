BINARY := git-agent
OUT := bin/$(BINARY)
PREFIX ?= ~/.local
BINDIR ?= $(PREFIX)/bin
XDG_CONFIG_HOME ?= $(HOME)/.config
FISH_CONFIG_DIR ?= $(XDG_CONFIG_HOME)/fish
FISH_COMPLETIONS_DIR ?= $(FISH_CONFIG_DIR)/completions
FISH_COMPLETION := completions/$(BINARY).fish

.PHONY: all build test install clean

all: build

build:
	go build -o $(OUT) -ldflags='-s -w' ./cmd/git-agent

test:
	go test ./...

install: build
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 $(OUT) $(DESTDIR)$(BINDIR)/$(BINARY)
	@if [ -d "$(DESTDIR)$(FISH_CONFIG_DIR)" ]; then \
		install -d "$(DESTDIR)$(FISH_COMPLETIONS_DIR)"; \
		install -m 0644 "$(FISH_COMPLETION)" "$(DESTDIR)$(FISH_COMPLETIONS_DIR)/$(BINARY).fish"; \
	fi

clean:
	rm -f $(OUT)
