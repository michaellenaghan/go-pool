BENCH_COUNT := 1
BENCH_CPU := ''
BENCH_CPU_PARALLEL := 1,2,4,8,16,32
BENCH_OPTS := -benchmem
BENCH_PATTERN := .
BENCH_SKIP := ''
BENCH_TIME := 1s
BENCH_TIMEOUT := 15m

TEST_COUNT := 1
TEST_CPU := ''
TEST_OPTS := -race -v
TEST_PATTERN := .
TEST_SKIP := ''

.NOTPARALLEL:

.PHONY: help
help:
	@echo "Targets:"
	@echo ""
	@echo "  bench-all        - Run all benchmarks"
	@echo "  bench-sequential - Run sequential benchmarks"
	@echo "  bench-parallel   - Run parallel benchmarks"
	@echo "  test-all         - Run all tests"
	@echo ""
	@echo "Variables:"
	@echo ""
	@echo "  BENCH_COUNT=$(BENCH_COUNT)"
	@echo "  BENCH_CPU=$(BENCH_CPU)"
	@echo "  BENCH_CPU_PARALLEL=$(BENCH_CPU_PARALLEL)"
	@echo "  BENCH_OPTS=\"$(BENCH_OPTS)\""
	@echo "  BENCH_PATTERN=$(BENCH_PATTERN)"
	@echo "  BENCH_SKIP=$(BENCH_SKIP)"
	@echo "  BENCH_TIME=$(BENCH_TIME)"
	@echo "  BENCH_TIMEOUT=$(BENCH_TIMEOUT)"
	@echo ""
	@echo "  TEST_COUNT=$(TEST_COUNT)"
	@echo "  TEST_CPU=$(TEST_CPU)"
	@echo "  TEST_OPTS=\"$(TEST_OPTS)\""
	@echo "  TEST_PATTERN=$(TEST_PATTERN)"
	@echo "  TEST_SKIP=$(TEST_SKIP)"
	@echo ""
	@echo "Examples:"
	@echo ""
	@echo "  make bench-all"
	@echo ""
	@echo "  make bench-sequential BENCH_COUNT=6"
	@echo ""
	@echo "  make bench-parallel BENCH_CPU_PARALLEL=4,8"
	@echo ""
	@echo "  make test-all"
	@echo ""
	@echo "  make test TEST_PATTERN=Example"
	@echo ""

.PHONY: all
all: bench-all test-all

.PHONY: bench
bench:
	go test \
		-bench "$(BENCH_PATTERN)" \
		-benchtime $(BENCH_TIME) \
		-count $(BENCH_COUNT) \
		-cpu $(BENCH_CPU) \
		-run "''" \
		-skip "$(BENCH_SKIP)" \
		-timeout $(BENCH_TIMEOUT) \
		$(BENCH_OPTS)

.PHONY: bench-all
bench-all: bench-sequential bench-parallel

.PHONY: bench-sequential
bench-sequential:
	$(MAKE) bench BENCH_PATTERN="/Sequential"

.PHONY: bench-parallel
bench-parallel:
	$(MAKE) bench BENCH_PATTERN="/Parallel" BENCH_CPU=$(BENCH_CPU_PARALLEL)

.PHONY: test
test:
	go test \
		-count $(TEST_COUNT) \
		-cpu $(TEST_CPU) \
		-run "$(TEST_PATTERN)" \
		-skip "$(TEST_SKIP)" \
		$(TEST_OPTS)

.PHONY: test-all
test-all: test
