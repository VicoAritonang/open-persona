// Cloudflare Worker for go.vicoworks.com: Go vanity import paths.
//
// `go get go.vicoworks.com/openpersona` asks this host where the code lives
// (a request with ?go-get=1) and reads the answer from a <meta> tag. People
// who open the URL in a browser are sent to the package documentation.
//
// To publish another module under this domain, add it to MODULES and deploy.

const HOST = "go.vicoworks.com";

const MODULES = {
  openpersona: "https://github.com/VicoAritonang/open-persona",
};

export default {
  async fetch(request) {
    const url = new URL(request.url);
    const name = url.pathname.split("/").filter(Boolean)[0];

    if (!name) {
      return html(indexPage());
    }
    const repo = MODULES[name];
    if (!repo) {
      return new Response("unknown module\n", { status: 404 });
    }

    const module = `${HOST}/${name}`;
    const docs = `https://pkg.go.dev/${HOST}${url.pathname.replace(/\/+$/, "")}`;
    if (url.searchParams.get("go-get") !== "1") {
      return Response.redirect(docs, 302);
    }
    // The prefix must be the module root even when a sub-package is requested.
    return html(`<!doctype html>
<meta charset="utf-8">
<meta name="go-import" content="${module} git ${repo}">
<meta name="go-source" content="${module} ${repo} ${repo}/tree/main{/dir} ${repo}/blob/main{/dir}/{file}#L{line}">
<a href="${docs}">${module}</a>
`);
  },
};

function html(body) {
  return new Response(body, {
    headers: {
      "content-type": "text/html; charset=utf-8",
      "cache-control": "public, max-age=3600",
    },
  });
}

function indexPage() {
  const items = Object.keys(MODULES)
    .map((n) => `<li><a href="https://pkg.go.dev/${HOST}/${n}"><code>${HOST}/${n}</code></a></li>`)
    .join("\n");
  return `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${HOST}</title>
<h1>Go modules</h1>
<ul>
${items}
</ul>
`;
}
