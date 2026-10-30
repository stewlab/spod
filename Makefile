.PHONY: all build install test test-real clean

all: build

build:
	@./build.sh build

test:
	@./build.sh test

test-real:
	@./build.sh test-real

install:
	@./build.sh install

clean:
	@./build.sh clean
