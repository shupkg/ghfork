install:
	CGO_ENABLED=0 go install

Test:install
	cd /Users/shu/Projects/test && ghfork
