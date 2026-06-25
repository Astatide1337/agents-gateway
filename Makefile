.PHONY: test lint compile smoke verify clean

compile:
	uv run python -m compileall agents_gateway/ tests/

test:
	uv run pytest tests/ -v

smoke:
	bash scripts/smoke-test.sh

verify: compile test
	@echo "Verification complete."

clean:
	rm -rf data/ .pytest_cache __pycache__ .venv
