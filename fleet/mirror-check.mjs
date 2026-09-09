// Asserts the client's fleet validator agrees with the Go one on every fixture.
// D6 says one ingress; two implementations of one ingress only stay one if a
// gate proves they refuse the same documents. Run: node relay-go/fleet/mirror-check.mjs
import { readFileSync, readdirSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const repo = join(here, "..", "..");

const html = readFileSync(join(repo, "index.html"), "utf8");
const start = html.indexOf("// --- fleet-validate:start ---");
const end = html.indexOf("// --- fleet-validate:end ---");
if (start < 0 || end < 0) {
  console.error("mirror-check: the fleet-validate markers are missing from index.html");
  process.exit(1);
}
const src = html.slice(start, end);
const fleetValidateText = new Function(src + "\nreturn fleetValidateText;")();

// The Go side prints one JSON array of issues per fixture, keyed by path.
const goOut = execFileSync("go", ["run", "./fleet/cmd/fleetcheck", "-json", "./fleet/testdata"], {
  cwd: join(repo, "relay-go"), encoding: "utf8",
});
const go = JSON.parse(goOut);

let checked = 0, bad = 0;
for (const dir of ["valid", "invalid", "secrets", "normalises"]) {
  for (const name of readdirSync(join(here, "testdata", dir)).filter((n) => n.endsWith(".json"))) {
    const key = `${dir}/${name}`;
    const text = readFileSync(join(here, "testdata", dir, name), "utf8");
    const jsIssues = fleetValidateText(text);
    const goIssues = go[key] ?? [];
    // path AND code, undeduplicated: comparing a deduplicated set of paths made a
    // differing `code` — and a differing count at one path — invisible to the very
    // gate D6 cites as the reason a rule cannot drift between the two ingresses.
    const jsPaths = jsIssues.map((i) => i.path + "|" + i.code).toSorted();
    const goPaths = goIssues.map((i) => i.path + "|" + i.code).toSorted();
    checked++;
    if (JSON.stringify(jsPaths) !== JSON.stringify(goPaths)) {
      bad++;
      console.error(`MISMATCH ${key}\n  go: ${JSON.stringify(goPaths)}\n  js: ${JSON.stringify(jsPaths)}`);
    }
  }
}
console.log(`mirror-check: ${checked} fixtures, ${bad} mismatch(es)`);
process.exit(bad === 0 ? 0 : 1);
