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
    <div class="home-bar-chart" role="list" aria-label="起動時間平均: Go組み込み日本語無効2.02ミリ秒、有効335.1ミリ秒。Docker 6.07秒、1GiB data tmpfs付きTestcontainers Go 6.48秒、Devbox services 7.94秒">
      <div class="home-bar-row" role="listitem"><span>Go組み込み · 日本語無効</span><strong>2.02 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 0.03%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Go組み込み · 日本語有効</span><strong>335.1 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 4.2%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Docker · OpenSearch</span><strong>6.07 s</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 76.5%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Testcontainers Go · OpenSearch、data tmpfs</span><strong>6.48 s</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 81.5%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Devbox <a href="https://www.jetify.com/docs/devbox/cli-reference/devbox-services-up"><code>services up -b</code></a> · OpenSearch</span><strong>7.94 s</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--devbox" style="--bar-size: 100%"></span></span></div>
    </div>
  </section>

  <section class="home-chart-card" aria-labelledby="home-memory-title">
    <h2 id="home-memory-title">server起動中のmemory</h2>
    <p>Docker rowはDocker報告のcontainer memory。Testcontainersはdata pathに1GiB tmpfsを使い、Go runnerのRSS増分だけを加算。</p>
    <div class="home-bar-chart" role="list" aria-label="ready時memory: osmem 162MiB、Docker 942MiB、tmpfs付きTestcontainers Go 956MiB、Devbox services 1009MiB">
      <div class="home-bar-row" role="listitem">
        <span>osmem server · 日本語有効</span><strong>162 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 16.0%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Docker · OpenSearch container</span><strong>942 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 93.4%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Testcontainers Go · container + runner増分、tmpfs</span><strong>956 MiB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 94.8%"></span></span>
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
    <div class="home-bar-chart" role="list" aria-label="clone生成時間平均: Go組み込み0.8マイクロ秒、Python server SDK 0.22ミリ秒、Java server SDK 0.51ミリ秒、Node.js server SDK 1.77ミリ秒">
      <div class="home-bar-row" role="listitem"><span>Go組み込み · <code>Cluster.Clone()</code></span><strong>0.8 µs</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 0.05%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Python · <code>server.clone()</code></span><strong>0.22 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 12.2%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Java · <code>server.clone()</code></span><strong>0.51 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 28.6%"></span></span></div>
      <div class="home-bar-row" role="listitem"><span>Node.js · <code>server.clone()</code></span><strong>1.77 ms</strong><span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 100%"></span></span></div>
    </div>
    <p>同日にroute table cache化の修正が入り、Go組み込みの<code>Clone()</code>はこのページで過去最速になりました。詳細は<a href="/osmem/ja/performance/#二つの回帰を修正し一つを保留">性能ページ</a>を参照してください。</p>
  </section>

  <section class="home-chart-card home-chart-card--full" aria-labelledby="home-footprint-title">
    <h2 id="home-footprint-title">link後binaryと初回downloadのfootprint</h2>
    <p>10進MBの同一scale。barは別々の対象を表し、package全体の同種比較ではありません。</p>
    <div class="home-bar-chart" role="list" aria-label="サイズ比較: osmemと日本語analyzerのGo app link増分43.8MB、Devbox MavenとJDK環境の初回download 203.5MB、Docker Hub上のOpenSearch arm64圧縮image 739.3MB">
      <div class="home-bar-row" role="listitem">
        <span>Go app · osmem + 日本語analyzerのlink増分 <small>(+41.8 MiB)</small></span><strong>43.8 MB</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 5.9%"></span></span>
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

<p class="home-method-note">2026-09-16に再計測しました。OpenSearch 3.8互換性修正と2つの性能改善(in-memory segment merge、typedなtop-K sort実行)を経た後の値です。before/afterは<a href="/osmem/ja/performance/">計測ページ</a>を参照してください。起動グラフはGo組み込みの日本語無効/有効を比較します。有効時はseedに含むkuromoji analyzerの初期化も含み、525-byte seedに対する<code>New()+LoadSeed()</code>を計測しています。Python・Java・Node.js SDKの起動値と範囲は<a href="/osmem/ja/performance/">計測ページ</a>にまとめました。cloneはGo 5,000回×5、SDK各300回で生成のみ計測。Devboxはprocess-compose経由で同じDocker-backed OpenSearchを起動し、初回downloadのMaven/JDK環境はserver imageと別です。container imageは取得済み。Testcontainersはdata pathに1GiB tmpfsをmountし、初回trialでRyukを起動。Docker報告のcontainer memoryはdaemonを除外し、Testcontainers Goは起動前からのrunner RSS増分を加算しています。ready条件が異なるため、厳密なengine比較ではなくローカルの経路比較です。</p>
