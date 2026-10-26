// Shared harness for the component unit tests: evaluate common.js then the
// named page script in a DOM-less Node VM context, mirroring the browser's
// script order (common.js defines window.kbm, the page script registers its
// component factory on window). Returns the sandbox so tests can grab the
// factory and stub window locals.
import { readFileSync } from "node:fs";
import vm from "node:vm";

export function evalComponent(fileName) {
  const context = { window: {} };
  for (const name of ["common.js", fileName]) {
    vm.runInNewContext(
      readFileSync(
        new URL(`../internal/server/static/${name}`, import.meta.url),
        "utf8",
      ),
      context,
    );
  }
  return context;
}
