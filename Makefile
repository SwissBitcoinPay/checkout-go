# The checks of scripts/check.sh, by name: `make check` runs them all.
CHECKS := spec format vet lint mod test build vuln

.PHONY: check $(CHECKS)

check:
	./scripts/check.sh

$(CHECKS):
	./scripts/check.sh $@
