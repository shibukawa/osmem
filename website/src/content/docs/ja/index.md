---
title: "テストごとに新しい状態を。container待ちなしで。"
description: "OpenSearch互換のテスト環境をcopy-on-writeで分離。サブ秒起動と小さなメモリ使用量を実測値で紹介します。"
template: splash
hero:
  tagline: OpenSearch互換のbaseを一度seedし、テストごとに独立したforkを渡す。公式clientはそのまま。重い検索containerの起動は要りません。
  actions:
    - text: はじめに
      link: /osmem/ja/getting-started/
      icon: right-arrow
    - text: 計測値を見る
      link: /osmem/ja/performance/
      icon: right-arrow
    - text: GitHub
      link: https://github.com/shibukawa/osmem
      icon: external
      variant: minimal
---

## テストごとに新しい状態を。検索engineは起動し直さない。

seed済みbaseを共有し、読み取り専用テストは直接検索できます。document、mapping、indexの状態を変えるテストには専用のcopy-on-write forkを渡します。テストが変更するindexだけが複製されます。

<div class="home-chart-grid">
  <section class="home-chart-card home-chart-card--full" aria-labelledby="home-startup-title">
    <h2 id="home-startup-title">seed済みtest環境の起動時間</h2>
    <p>各経路の平均。osmemは525-byte seedを読み込みreadyになるまで、container系はwarm imageから<code>PUT /benchmark</code>成功まで。</p>
    <div class="home-bar-chart" role="list" aria-label="起動時間平均: Go組み込み日本語無効2.13ミリ秒、有効319.7ミリ秒。Docker 6.07秒、Testcontainers Go 6.26秒、Devbox services 7.94秒">
      <div class="home-bar-row" role="listitem"><span>Go組み込み · 日本語無効</span><strong>2.13 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 0.03%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Go組み込み · 日本語有効</span><strong>319.7 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 4.0%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Docker · OpenSearch</span><strong>6.07 s</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 76.5%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Testcontainers Go · OpenSearch</span><strong>6.26 s</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 78.8%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Devbox <a href="https://www.jetify.com/docs/devbox/cli-reference/devbox-services-up"><code>services up -b</code></a> · OpenSearch</span><strong>7.94 s</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--devbox" style="--bar-size: 100%"></span></span></div>
    </div>
  </section>

  <section class="home-chart-card" aria-labelledby="home-memory-title">
    <h2 id="home-memory-title">server起動中のmemory</h2>
    <p>RSS平均。TestcontainersはGo runnerの起動前からの増分だけを加算。</p>
    <div class="home-bar-chart" role="list" aria-label="RSS平均: osmem 160MiB、Docker 942MiB、Testcontainers Go 958MiB、Devbox services 1009MiB">
      <div class="home-bar-row" role="listitem">
        <span>osmem server · 日本語有効</span><strong>160 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 15.9%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Docker · OpenSearch container</span><strong>942 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 93.4%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Testcontainers Go · container + runner増分</span><strong>958 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 94.9%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Devbox · container + service process</span><strong>1,009 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--devbox" style="--bar-size: 100%"></span></span>
      </div>
    </div>
  </section>

  <section class="home-chart-card" aria-labelledby="home-clone-title">
    <h2 id="home-clone-title">変更テスト用cloneの生成時間</h2>
    <p>構築済みの日本語有効indexをforkする時間。writeやcloseは含めません。</p>
    <div class="home-bar-chart" role="list" aria-label="clone生成時間平均: Go組み込み14.4マイクロ秒、Python server SDK 0.24ミリ秒、Java server SDK 0.58ミリ秒、Node.js server SDK 1.63ミリ秒">
      <div class="home-bar-row" role="listitem"><span>Go組み込み · <code>Cluster.Clone()</code></span><strong>14.4 µs</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 0.88%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Python · <code>server.clone()</code></span><strong>0.24 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 14.7%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Java · <code>server.clone()</code></span><strong>0.58 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 35.8%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Node.js · <code>server.clone()</code></span><strong>1.63 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 100%"></span></span></div>
    </div>
  </section>

  <section class="home-chart-card home-chart-card--full" aria-labelledby="home-footprint-title">
    <h2 id="home-footprint-title">link後binaryと初回downloadのfootprint</h2>
    <p>10進MBの同一scale。barは別々の対象を表し、package全体の同種比較ではありません。</p>
    <div class="home-bar-chart" role="list" aria-label="サイズ比較: osmemと日本語analyzerのGo app link増分36.7MB、Devbox MavenとJDK環境の初回download 203.5MB、Docker Hub上のOpenSearch arm64圧縮image 739.3MB">
      <div class="home-bar-row" role="listitem">
        <span>Go app · osmem + 日本語analyzerのlink増分 <small>(+35.0 MiB)</small></span><strong>36.7 MB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 5%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Devbox · Maven/JDK環境の初回download <small>(194.1 MiB)</small></span><strong>203.5 MB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--devbox" style="--bar-size: 27.5%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>OpenSearch 2.19.0 · <a href="https://hub.docker.com/r/opensearchproject/opensearch/tags?name=2.19.0">Docker Hub圧縮値</a>、linux/arm64</span><strong>739.3 MB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 100%"></span></span>
      </div>
    </div>
  </section>
</div>

<p class="home-method-note">起動グラフはGo組み込みの日本語無効/有効を比較します。有効時はseedに含むkuromoji analyzerの初期化も含み、525-byte seedに対する<code>New()+LoadSeed()</code>を計測しています。Python・Java・Node.js SDKの起動値と範囲は<a href="/osmem/ja/performance/">計測ページ</a>にまとめました。cloneはGo 5,000回×5、SDK各300回で生成のみ計測。Devboxはprocess-compose経由で同じDocker-backed OpenSearchを起動し、初回downloadのMaven/JDK環境はserver imageと別です。container imageは取得済み、Testcontainersの初回はRyukも起動。RSSにDocker daemonは含めず、Testcontainers Goは起動前からのrunner RSS増分、Devboxはprocess-composeとDocker clientを加算しています。ready条件が異なるため、厳密なengine比較ではなくローカルの経路比較です。</p>
