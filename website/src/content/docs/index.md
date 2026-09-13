---
title: "Fresh search state. No container wait."
description: "OpenSearch-compatible tests with an isolated copy-on-write cluster for every test, sub-second startup, and a small runtime footprint."
template: splash
hero:
  tagline: Seed one OpenSearch-compatible base. Give every test its own isolated fork. Keep the official client; skip the heavyweight search container.
  actions:
    - text: Get started
      link: /osmem/getting-started/
      icon: right-arrow
    - text: See the measurements
      link: /osmem/performance/
      icon: right-arrow
    - text: GitHub
      link: https://github.com/shibukawa/osmem
      icon: external
      variant: minimal
---

<div class="home-proof-grid" role="list" aria-label="osmem local benchmark highlights">
  <article class="home-proof-card home-proof-card--lead" role="listitem">
    <span class="home-proof-label">Japanese-enabled startup · 5-run average</span>
    <strong>680 <small>ms</small></strong>
    <span class="home-proof-detail">Seed loaded, analyzer exercised</span>
  </article>
  <article class="home-proof-card" role="listitem">
    <span class="home-proof-label">Server memory · average RSS</span>
    <strong>160 <small>MiB</small></strong>
    <span class="home-proof-detail">Small Japanese seed, macOS arm64</span>
  </article>
  <article class="home-proof-card" role="listitem">
    <span class="home-proof-label">Linked Go app · osmem + Japanese</span>
    <strong>+35.0 <small>MiB</small></strong>
    <span class="home-proof-detail">Increment over a minimal Go executable</span>
  </article>
  <article class="home-proof-card" role="listitem">
    <span class="home-proof-label">OpenSearch image · Docker Hub compressed</span>
    <strong>739 <small>MB</small></strong>
    <span class="home-proof-detail">Download footprint; cache can reduce transfer</span>
  </article>
</div>

## One test, one fresh state—not one fresh search engine

osmem starts a seeded base once and forks it for each test. A fork initially shares its indexes; only the index a test changes is copied. OpenSearch containers can also be shared at class scope, but they do not provide this built-in copy-on-write isolation.

<div class="home-chart-grid">
  <section class="home-chart-card" aria-labelledby="home-startup-title">
    <h2 id="home-startup-title">Average time to a ready test server</h2>
    <p>Five local trials per path; lower is faster.</p>
    <div class="home-bar-chart" role="list" aria-label="Average startup: osmem 0.68 seconds, Docker 6.07 seconds, Testcontainers 8.12 seconds">
      <div class="home-bar-row" role="listitem">
        <span>osmem · Japanese enabled</span><strong>0.68 s</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 8.4%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Docker · OpenSearch</span><strong>6.07 s</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 74.8%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Testcontainers · OpenSearch</span><strong>8.12 s</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 100%"></span></span>
      </div>
    </div>
  </section>

  <section class="home-chart-card" aria-labelledby="home-memory-title">
    <h2 id="home-memory-title">Memory while the server is ready</h2>
    <p>Resident memory, not configured heap; the Testcontainers total includes its test JVM.</p>
    <div class="home-bar-chart" role="list" aria-label="Average resident memory: osmem 160 mebibytes, Docker OpenSearch 942 mebibytes, Testcontainers with test JVM 1031 mebibytes">
      <div class="home-bar-row" role="listitem">
        <span>osmem server</span><strong>160 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 15.5%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Docker · OpenSearch container</span><strong>942 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 91.4%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Testcontainers · container + test JVM</span><strong>1,031 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 100%"></span></span>
      </div>
    </div>
  </section>
</div>

<aside class="home-devbox-note">
  <strong>Devbox is the toolbox, not the search server.</strong>
  <span>A warm <code>devbox run</code> added 156 ms on average. The temporary Maven/JDK environment downloaded 194.1 MiB once (351.7 MiB unpacked); Devbox itself does not start OpenSearch.</span>
</aside>

<p class="home-method-note">Local averages, not a controlled engine ranking. Docker’s image was already pulled; Testcontainers started a fresh container per trial, while many suites amortize one container across a class. osmem’s RSS includes the Japanese analyzer. <a href="/osmem/performance/">See all conditions and measurements</a>.</p>
