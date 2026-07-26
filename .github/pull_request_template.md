## What changed and why

<!-- One or two sentences. -->

## Evidence (fill in what applies — delete rows that genuinely don't)

- [ ] **Tests executed**: link to the CI run — <!-- URL -->
- [ ] **Image digest / release commit**: <!-- sha256:... or 40-char commit -->
- [ ] **Vulnerability scan result**: <!-- pass/fail, severity gate, link to the Trivy report artifact -->
- [ ] **Staging health verification**: <!-- link to astatide-deploy's audit log entry or the deploy-staging.yml run -->
- [ ] **Cross-environment auth verification**: <!-- link to astatide-staging verify output, if this PR touches auth/isolation -->
- [ ] **Rollback target**: <!-- the previous digest/commit this would roll back to if reverted -->
- [ ] Screenshots or logs (if relevant): <!-- attach or link -->

## Unresolved risks

<!-- Required field — do not leave this as "None" without stating why
     you believe that's actually true. If this PR has no deployment
     impact at all, say so explicitly instead of leaving this blank. -->
