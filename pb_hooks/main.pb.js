/// <reference path="../pb_data/types.d.ts" />

// Example JS hook — a tiny custom route to prove the extension point works.
// Hit it at: GET http://localhost:8090/api/hello
routerAdd("GET", "/api/hello", (e) => {
  return e.json(200, { message: "pocketbase is extendable", time: new Date().toString() });
});
