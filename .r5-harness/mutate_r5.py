import os, re, subprocess, sys

ROOT = "/tmp/roborev-mut"
ENV = dict(os.environ, ROBOREV_TELEMETRY_ENABLED="0", GIT_CONFIG_GLOBAL="/tmp/r5-gitconfig")
ENV.pop("ROBOREV_HOST_AGENT", None)

A, D, C, T, CFG = "./internal/agent/", "./internal/daemon/", "./cmd/roborev/", "./cmd/roborev/tui/", "./internal/config/"
H = "internal/agent/host.go"
W = "internal/daemon/worker.go"

MUTATIONS = [
    ("RULING auto picks never use host, even with =1 (non-strict last resort)",
     [(H, "\tif !m.Allows(HostAuto) {\n\t\treturn nil, withSkippedHostNote(noAgentErr, backups)", "\tif true {\n\t\treturn nil, withSkippedHostNote(noAgentErr, backups)")],
     [(A, "TestHostIsLastResortOnlyWithExplicitMarker|TestModelSourceSelection"), (D, "TestHostRulingByMarkerMode")]),
    ("RULING auto backup never uses host, even with =1",
     [("internal/agent/auto.go", "if lastResort && usable(hostAgentName) && HostAllowed(HostAuto) {", "if false {")],
     [(A, "TestResolveAutoBackup"), (D, "TestHostRulingByMarkerMode|TestHostFailoverByMarkerMode")]),
    ("RULING detection makes host an automatic pick and a listing",
     [(H, "\treturn m.Explicit\n}\n\n// HostAllowed", "\treturn m.Explicit || m.Detected\n}\n\n// HostAllowed")],
     [(A, "TestHostMarkerAllows|TestHostIsLastResortOnlyWithExplicitMarker|TestResolveAutoBackup"), (D, "TestHostRulingByMarkerMode|TestHostFailoverByMarkerMode|TestAutoPinnedHostIsRecheckedOnRestart")]),
    ("RULING detection ignored when host is named",
     [(H, "\t\treturn m.Explicit || m.Detected\n\t}", "\t\treturn m.Explicit\n\t}")],
     [(A, "TestHostMarkerAllows|TestExplicitHostAgent|TestBackupHostWithMarkerIsUsed"), (D, "TestHostRulingByMarkerMode|TestAutoPinnedHostIsRecheckedOnRestart|TestHostFailoverByMarkerMode")]),
    ("RULING auto backup last resort accepts detection",
     [("internal/agent/auto.go", "usable(hostAgentName) && HostAllowed(HostAuto)", "usable(hostAgentName) && HostAllowed(HostNamed)")],
     [(A, "TestResolveAutoBackup"), (D, "TestHostFailoverByMarkerMode")]),
    ("RULING auto backup order loop no longer skips host",
     [("internal/agent/auto.go", 'name != "test" && name != hostAgentName && usable(name)', 'name != "test" && usable(name)')],
     [(A, "TestResolveAutoBackup|TestModelSourceSelection"), (D, "TestHostRulingByMarkerMode|TestHostFailoverByMarkerMode")]),
    ("RULING strict enqueue with an auto backup takes host",
     [("internal/agent/acp_resolution.go", "resolveAutoBackup(repoCfg, cfg, preferred, nil, false)", "resolveAutoBackup(repoCfg, cfg, preferred, nil, true)")],
     [(A, "TestModelSourceSelection"), (D, "TestHostRulingByMarkerMode")]),
    ("NO-MARKER failover error no longer names ROBOREV_HOST_AGENT (auto)",
     [("internal/agent/auto.go", "func AutoBackupNotFound() string {\n\tif HostAllowed(HostAuto) {", "func AutoBackupNotFound() string {\n\tif true {")],
     [(A, "TestResolveAutoBackup"), (D, "TestHostFailoverByMarkerMode")]),
    ("NO-MARKER failover error no longer names ROBOREV_HOST_AGENT (named host backup)",
     [(H, "\tif HostAllowed(HostNamed) || !slices.ContainsFunc(backups, isHostName) {", "\tif true {")],
     [(A, "TestBackupHostWithoutMarker"), (D, "TestHostFailoverByMarkerMode")]),
    ("NO-MARKER worker drops the auto note",
     [(W, "\t\t\treturn agent.AutoBackupNotFound()", '\t\t\treturn ""')],
     [(D, "TestHostFailoverByMarkerMode")]),
    ("P2-1 auto-pinned host dropped on restart even with =1",
     [(W, '\t\tagent.CanonicalName(job.BackupAgent) == "host" && !agent.HostAllowed(agent.HostAuto)) {', '\t\tagent.CanonicalName(job.BackupAgent) == "host") {')],
     [(D, "TestAutoPinnedHostIsRecheckedOnRestart")]),
    ("P2-1 auto-pinned host re-checked against named rule (detection runs it)",
     [(W, '\t\tagent.CanonicalName(job.BackupAgent) == "host" && !agent.HostAllowed(agent.HostAuto)) {', '\t\tagent.CanonicalName(job.BackupAgent) == "host" && !agent.HostAllowed(agent.HostNamed)) {')],
     [(D, "TestAutoPinnedHostIsRecheckedOnRestart")]),
    ("P2-1 auto-pinned host not re-checked on restart",
     [(W, '\t\tagent.CanonicalName(job.BackupAgent) == "host" && !agent.HostAllowed(agent.HostAuto)) {', "\t\tfalse) {")],
     [(D, "TestAutoPinnedHostIsRecheckedOnRestart")]),
    ("P2-1/F02 backup_auto not stored",
     [("internal/storage/jobs.go", "nullString(opts.AnalysisCommitSHA), opts.BackupAuto)", "nullString(opts.AnalysisCommitSHA), false)")],
     [(D, "TestEmptyAutoPinIsFrozen|TestHostFailoverByMarkerMode|TestFixJobPinsAutoBackup|TestGoalReviewPinsAutoBackup"), ("./internal/storage/", ".")]),
    ("P2-1/F02 backup_auto not read back",
     [("internal/storage/job_columns.go", "&j.BudgetRoutingLocked, &j.BackupAuto,", "&j.BudgetRoutingLocked, new(bool),")],
     [(D, "TestEmptyAutoPinIsFrozen|TestHostFailoverByMarkerMode|TestFixJobPinsAutoBackup|TestGoalReviewPinsAutoBackup")]),
    ("F02 empty auto pin re-resolved at failover",
     [(W, '\tif job.BackupAuto && (job.BackupAgent == "" ||', "\tif job.BackupAuto && (false ||")],
     [(D, "TestEmptyAutoPinIsFrozen|TestHostFailoverByMarkerMode")]),
    ("F02 empty-pin note dropped",
     [(W, '\tif job.BackupAuto && job.BackupAgent == "" {\n\t\treturn fmt.Sprintf(', '\tif false && job.BackupAuto && job.BackupAgent == "" {\n\t\treturn fmt.Sprintf(')],
     [(D, "TestEmptyAutoPinIsFrozen|TestHostFailoverByMarkerMode")]),
    ("F02 fix enqueue does not record auto",
     [("internal/daemon/server.go", "\t\tBackupAuto:   agentname.IsAuto(resolution.BackupAgent),\n", "")],
     [(D, "TestFixJobPinsAutoBackup")]),
    ("F02 goal enqueue does not record auto",
     [("internal/daemon/goal_review.go", "BackupAuto: agentname.IsAuto(resolution.BackupAgent), ", "BackupAuto: false && agentname.IsAuto(resolution.BackupAgent), ")],
     [(D, "TestGoalReviewPinsAutoBackup")]),
    ("F02 review/experiment enqueue does not record auto",
     [("internal/daemon/server.go", "\t\to.BackupAuto = backupIsAuto(in.repoCfg, in.cfg, in.workflow)\n", "")],
     [(D, "AutoBackup|AutoPin|HostRuling|Experiment")]),
    ("F02 panel enqueue does not record auto",
     [("internal/daemon/panel_enqueue.go", "\t\to.BackupAuto = backupIsAuto(repoCfg, cfg, workflowForPanelReviewType(m.ReviewType))\n", "")],
     [(D, "AutoBackup|AutoPin|Panel")]),
    ("F02 CI member opts do not record auto",
     [("internal/daemon/ci_poller.go", "\t\t\tBackupAuto:            backupIsAuto(in.repoCfg, in.cfg, config.WorkflowForReviewType(m.ReviewType)),\n", "")],
     [(D, "AutoBackup|AutoPin|CIMatrix|BuildPanelOpts|CIPoller")]),
    ("F06 CI auto backup ignores review-type eligibility",
     [("internal/daemon/ci_poller.go", "selectedAgent, autoBackupEligible(reviewType))", "selectedAgent, nil)")],
     [(D, "AutoBackup|CIMatrix|GoalReview")]),
    ("F06 worker auto backup ignores review-type eligibility",
     [(W, "job.Agent, autoBackupEligible(job.ReviewType))", "job.Agent, nil)")],
     [(D, "AutoBackup|GoalReview|AutoPin")]),
    ("F06 goal eligibility check removed",
     [(W, "\t\t\treturn goalreview.ValidateAgent(a) == nil", "\t\t\treturn true")],
     [(D, "TestGoalReviewPinsAutoBackup")]),
    ("F06 structured-review eligibility check removed",
     [(W, "\t\treturn agent.ValidateStructuredReviewSelection(reviewType, a) == nil", "\t\treturn true")],
     [(D, "AutoBackup|Custom"), (A, "TestAutoBackupForCustomReview")]),
    ("F03 --agent auto passes the review model through",
     [("internal/agent/model_resolution.go", '\tif agentname.IsAuto(w.PreferredAgent) && strings.TrimSpace(cliModel) == "" {', '\tif false && agentname.IsAuto(w.PreferredAgent) && strings.TrimSpace(cliModel) == "" {')],
     [(A, "TestModelSourceSelection")]),
    ("F03 workflow auto passes the workflow model through",
     [("internal/agent/model_resolution.go", "\tif agentname.IsAuto(config.ResolveAgentForWorkflowFromConfig(\"\", repoCfg, globalCfg, workflow, level)) {", "\tif false {")],
     [(A, "TestModelSourceSelection")]),
    ("F03 auto pairs default_model with any agent",
     [("internal/agent/model_resolution.go", "\t\tworkflowModelComparableAgentNameFromConfig(defaultAgent, repoCfg, globalCfg) {\n\t\treturn \"\"\n\t}\n\treturn config.ResolveModelFromConfig", "\t\tworkflowModelComparableAgentNameFromConfig(defaultAgent, repoCfg, globalCfg) && false {\n\t\treturn \"\"\n\t}\n\treturn config.ResolveModelFromConfig")],
     [(A, "TestModelSourceSelection")]),
    ("F07 synthesis: non-host errors take the host path (no retry)",
     [("internal/daemon/synthesis_worker.go", '\t\tif agent.CanonicalName(agentName) == "host" {', "\t\tif true {")],
     [(D, "TestSynthesisNonHostErrorIsStillRetried|Synthesis")]),
    ("F07 synthesis: host timeout retried",
     [("internal/daemon/synthesis_worker.go", '\t\tif agent.CanonicalName(agentName) == "host" {', "\t\tif false {")],
     [(D, "TestSynthesisHostTimeoutSkipsRetries")]),
    ("BISECT check-agents smoke-tests host",
     [("cmd/roborev/check_agents.go", '\t\t\t\tif name == "host" {', "\t\t\t\tif false {")],
     [(C, "TestCheckAgentsNeverSmokeTestsHost")]),
    ("BISECT budget config accepts host",
     [("internal/config/budget.go", '\t\tif canonical == "host" {', "\t\tif false {")],
     [(CFG, "TestBudget")]),
    ("BISECT budget routing considers a host primary",
     [("internal/daemon/budget.go", '!cfg.Budget.Enabled || canonical == "host" {', "!cfg.Budget.Enabled {")],
     [(D, "TestBudgetRoutingNeverSelectsHost")]),
    ("P2-2 listing shows host under detection",
     [(H, "\tnames := Available()\n\tif HostAllowed(HostListed) {", "\tnames := Available()\n\tif HostAllowed(HostNamed) {")],
     [(A, "TestHostHiddenFromSelectionAndListings"), (T, "TestRerunAgentPickerListsHostOnlyWithMarker"), (C, "TestAgentFlagCompletionListsHostOnlyWithMarker|TestCheckAgentsNeverSmokeTestsHost|TestDoctorAgents")]),
    ("P2-2 doctor lists host under detection",
     [("cmd/roborev/doctor_checks.go", "if agent.HostAllowed(agent.HostListed) {", "if agent.HostAllowed(agent.HostNamed) {")],
     [(C, "TestDoctorAgents")]),
    ("P2-2 availability gate removed",
     [("internal/agent/agent.go", "\t\treturn HostAllowed(HostNamed)", "\t\treturn true"),
      ("internal/agent/acp_resolution.go", "\t\treturn HostAllowed(HostNamed)", "\t\treturn true")],
     [(A, "TestHostAvailabilityIsMarkerGated|TestBackupHostWithoutMarkerIsSkipped|TestExplicitHostWithoutMarkerErrors"), (D, "TestHostRulingByMarkerMode|TestStoredHostBackupNeedsMarker")]),
    ("F08 doctor mismatch never reports host",
     [("cmd/roborev/doctor_agents.go", "\t\t\tif !env.viewAgent(d.Name).diag.Available {", "\t\t\tif false {")],
     [(C, "TestDoctorAgents")]),
    ("F08 doctor mismatch always reports host (false warning)",
     [("cmd/roborev/doctor_agents.go", "\t\t\tif !env.viewAgent(d.Name).diag.Available {", "\t\t\tif true {")],
     [(C, "TestDoctorAgents")]),
    ("P2-2 doctor counts no named host when the daemon lists none",
     [("cmd/roborev/doctor_agents.go", 'if len(installed) == 0 && slices.Contains(doctorAgentNames(env), "host") {', 'if false && len(installed) == 0 && slices.Contains(doctorAgentNames(env), "host") {')],
     [(C, "TestDoctorAgents")]),
    ("P2-2 doctor counts host even when not named",
     [("cmd/roborev/doctor_agents.go", 'if len(installed) == 0 && slices.Contains(doctorAgentNames(env), "host") {', 'if len(installed) == 0 && (true || slices.Contains(doctorAgentNames(env), "host")) {')],
     [(C, "TestDoctorAgents")]),
    ("P3 HostUnavailableError matched only unwrapped",
     [(H, "\t_, ok := errors.AsType[*HostUnavailableError](err)", "\t_, ok := err.(*HostUnavailableError)")],
     [(A, "TestHostUnavailableSurvivesWrapping"), (D, "TestWorkerExplicitHostWithoutMarkerFailsWithoutRetry|TestSynthesisExplicitHostWithoutMarkerFailsWithoutRetry")]),
    ("P3 marker 0 no longer turns detection off",
     [(H, "\t\tcase \"1\", \"true\", \"yes\", \"on\":\n\t\t\treturn HostMarker{Explicit: true, Why: HostMarkerEnv + \"=\" + v}\n\t\t}\n\t\treturn HostMarker{}", "\t\tcase \"1\", \"true\", \"yes\", \"on\":\n\t\t\treturn HostMarker{Explicit: true, Why: HostMarkerEnv + \"=\" + v}\n\t\t}")],
     [(A, "TestReadHostMarker|TestHostMarkerAllows|TestExplicitHostAgent"), (D, "TestHostRulingByMarkerMode|TestAutoPinnedHostIsRecheckedOnRestart")]),
    ("P3 detection without CLOUD_AGENT_*",
     [(H, '\t\tif strings.HasPrefix(kv, "CLOUD_AGENT_") {', '\t\tif true || strings.HasPrefix(kv, "CLOUD_AGENT_") {')],
     [(A, "TestReadHostMarker")]),
    ("P3 requestedHostError removed",
     [(H, "\tif resolveAlias(strings.TrimSpace(name)) != hostAgentName || HostAllowed(HostNamed) {", "\tif true {")],
     [(A, "TestExplicitHostWithoutMarkerErrors"), (D, "TestEnqueueExplicitHostWithoutMarkerFailsFast|TestWorkerExplicitHostWithoutMarkerFailsWithoutRetry")]),
    ("DEFAULT listings leak host without the marker",
     [(H, "\tnames := Available()\n\tif HostAllowed(HostListed) {", "\tnames := Available()\n\tif true {")],
     [(D, "TestDefaultPathUnchanged"), (A, "TestHostHiddenFromSelectionAndListings")]),
    ("DEFAULT completion fallback leaks host",
     [("cmd/roborev/completions.go", "agent.ListedAgentNames(), cobra", "agent.Available(), cobra")],
     [(C, "TestAgentFlagCompletion")]),
    ("DEFAULT skipped-host note on every failure without the marker",
     [(H, "\tif HostAllowed(HostNamed) || !slices.ContainsFunc(backups, isHostName) {", "\tif HostAllowed(HostNamed) {")],
     [(D, "TestDefaultPathUnchanged")]),
    ("DEFAULT get-agent errors all fail without retry",
     [(W, "\tif !agent.IsHostUnavailable(err) {", "\tif false {")],
     [(D, "TestDefaultPathUnchanged|GetAgent|Synthesis|Retry")]),
    ("DEFAULT doctor mismatch wording changed",
     [("cmd/roborev/doctor_agents.go", '\t\twhat = "installed"\n', "")],
     [(C, "TestDoctorAgentsDaemonPathMismatch")]),
    ("DEFAULT doctor review summary drops the path form",
     [("cmd/roborev/doctor_agents.go", "\t\tif doctorIsHost(primary.diag.Name) {", "\t\tif true {")],
     [(C, "TestDoctorReviewSummaryKeepsPathForm")]),
    ("DEFAULT CI pins a backup when none is auto",
     [("internal/daemon/ci_poller.go", "\tif agentname.IsAuto(backupAgent) {\n\t\tname := agent.ResolveAutoBackup", "\tif true {\n\t\tname := agent.ResolveAutoBackup")],
     [(D, "TestDefaultPathUnchanged")]),
    ("DEFAULT auto model rule applied to every agent",
     [("internal/agent/model_resolution.go", '\tif agentname.IsAuto(w.PreferredAgent) && strings.TrimSpace(cliModel) == "" {', '\tif strings.TrimSpace(cliModel) == "" {')],
     [(D, "TestDefaultPathUnchanged"), (A, "TestModelSourceSelection")]),
]


def run(cmd, timeout=600):
    return subprocess.run(cmd, cwd=ROOT, env=ENV, capture_output=True, text=True, timeout=timeout)


only = sys.argv[1] if len(sys.argv) > 1 else ""
results = []
for title, edits, tests in MUTATIONS:
    if only and only not in title:
        continue
    run(["git", "checkout", "-q", "--", "."])
    applied = True
    for path, old, new in edits:
        p = os.path.join(ROOT, path)
        s = open(p).read()
        if old not in s:
            applied = False
            print("NOT APPLIED", title, path, flush=True)
            break
        open(p, "w").write(s.replace(old, new, 1))
    if not applied:
        results.append((title, "not applied", []))
        continue
    reds = []
    for pkg, regex in tests:
        r = run(["go", "test", "-count=1", "-timeout", "120s", pkg, "-run", regex])
        out = r.stdout + r.stderr
        failed = sorted(set(re.findall(r"^--- FAIL: (\S+)", out, re.M)))
        if r.returncode != 0 and not failed:
            failed = ["<build/other failure: " + out.strip().splitlines()[-1] + ">"]
        reds += [f"{pkg} {name}" for name in failed]
    results.append((title, "RED" if reds else "still green", reds))
    print(f"## {title}: {results[-1][1]}", flush=True)
    for r in reds:
        print(f"   - {r}", flush=True)
run(["git", "checkout", "-q", "--", "."])
print("\nSUMMARY")
for title, status, reds in results:
    print(f"## {title}: {status}")
    for r in reds:
        print(f"   - {r}")
