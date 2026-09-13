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

## A fresh state for every test—not a fresh search engine

Reuse one seeded base. Read-only tests can query it directly; a test that changes documents, mappings, or index state gets its own copy-on-write fork. Only an index the test mutates is copied.

<div class="home-chart-grid">
  <section class="home-chart-card home-chart-card--full" aria-labelledby="home-startup-title">
    <h2 id="home-startup-title">Average time to a seeded test environment</h2>
    <p>Local means. osmem loads the 525-byte seed; container paths use a warm image and stop at a successful <code>PUT /benchmark</code>.</p>
    <div class="home-bar-chart" role="list" aria-label="Average startup: Go embedded Japanese off 2.13 milliseconds and on 319.7 milliseconds; Docker 6.07 seconds; Go Testcontainers 6.26 seconds; Devbox services 7.94 seconds">
      <div class="home-bar-row" role="listitem"><span>Go embedded · Japanese off</span><strong>2.13 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 0.03%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Go embedded · Japanese on</span><strong>319.7 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 4.0%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Docker · OpenSearch</span><strong>6.07 s</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 76.5%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Testcontainers Go · OpenSearch</span><strong>6.26 s</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 78.8%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Devbox <a href="https://www.jetify.com/docs/devbox/cli-reference/devbox-services-up"><code>services up -b</code></a> · OpenSearch</span><strong>7.94 s</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--devbox" style="--bar-size: 100%"></span></span></div>
    </div>
  </section>

  <section class="home-chart-card" aria-labelledby="home-memory-title">
    <h2 id="home-memory-title">Memory while the server is ready</h2>
    <p>Average RSS. Testcontainers adds only its Go runner increase over the pre-start baseline.</p>
    <div class="home-bar-chart" role="list" aria-label="Average resident memory: osmem 160 mebibytes, Docker 942 mebibytes, Testcontainers Go 958 mebibytes, Devbox services 1009 mebibytes">
      <div class="home-bar-row" role="listitem">
        <span>osmem server · Japanese enabled</span><strong>160 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 15.9%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Docker · OpenSearch container</span><strong>942 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 93.4%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Testcontainers Go · container + runner increase</span><strong>958 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 94.9%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Devbox · container + service processes</span><strong>1,009 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--devbox" style="--bar-size: 100%"></span></span>
      </div>
    </div>
  </section>

  <section class="home-chart-card" aria-labelledby="home-clone-title">
    <h2 id="home-clone-title">Clone creation for mutating tests</h2>
    <p>Forking an already-built Japanese-enabled index. Writes and cleanup are excluded.</p>
    <div class="home-bar-chart" role="list" aria-label="Average clone creation: Go embedded 14.4 microseconds, Python server SDK 0.24 milliseconds, Java server SDK 0.58 milliseconds, Node.js server SDK 1.63 milliseconds">
      <div class="home-bar-row" role="listitem"><span>Go embedded · <code>Cluster.Clone()</code></span><strong>14.4 µs</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 0.88%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Python · <code>server.clone()</code></span><strong>0.24 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 14.7%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Java · <code>server.clone()</code></span><strong>0.58 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 35.8%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Node.js · <code>server.clone()</code></span><strong>1.63 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 100%"></span></span></div>
    </div>
  </section>

  <section class="home-chart-card home-chart-card--full" aria-labelledby="home-footprint-title">
    <h2 id="home-footprint-title">Linked binary and first-download footprint</h2>
    <p>Same decimal-MB scale; these bars describe different things, not interchangeable package sizes.</p>
    <div class="home-bar-chart" role="list" aria-label="Size comparison: linked Go application increase with osmem and Japanese analyzer 36.7 megabytes, first Devbox Maven and JDK environment download 203.5 megabytes, Docker Hub compressed OpenSearch arm64 image 739.3 megabytes">
      <div class="home-bar-row" role="listitem">
        <span>Go app · osmem + Japanese analyzer linked increase <small>(+35.0 MiB)</small></span><strong>36.7 MB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 5%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Devbox · first Maven/JDK environment download <small>(194.1 MiB)</small></span><strong>203.5 MB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--devbox" style="--bar-size: 27.5%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>OpenSearch 2.19.0 · <a href="https://hub.docker.com/r/opensearchproject/opensearch/tags?name=2.19.0">Docker Hub compressed</a>, linux/arm64</span><strong>739.3 MB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 100%"></span></span>
      </div>
    </div>
  </section>
</div>

<p class="home-method-note">The Go startup chart shows the embedded path: Japanese-disabled measures initialization without kuromoji, while Japanese-enabled includes kuromoji analyzer initialization. Its timer covers <code>New()+LoadSeed()</code> for the 525-byte seed. Python, Java, and Node.js SDK startup results and ranges are on the <a href="/osmem/performance/">measurement page</a>. Clone timing measures creation only: Go 5,000 × 5; each SDK 300 clones. Devbox starts the same Docker-backed OpenSearch service through process-compose; its first environment download is the Maven/JDK toolchain, not the server image. Server container paths use a cached image; the first Testcontainers trial also starts Ryuk. RSS excludes Docker daemon; Testcontainers Go adds only runner RSS increase over its pre-start baseline. Readiness conditions differ, so this is a local path comparison, not a controlled engine ranking.</p>
