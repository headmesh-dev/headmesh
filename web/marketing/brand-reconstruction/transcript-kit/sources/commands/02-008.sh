wc -c README.md docs/* sandbox/docs/* plugins/README.md CONTRIBUTING.md SECURITY.md 2>/dev/null; echo; find web -type f | grep -v node_modules | head -80
