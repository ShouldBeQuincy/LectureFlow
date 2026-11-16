// Not Go code. This file only marks desktop/ as a separate module so that
// the Go tools of the repository's module (go list / vet / test ./...) never
// look into desktop/node_modules, where npm packages may ship Go files.
module lectureflow.invalid/desktop
