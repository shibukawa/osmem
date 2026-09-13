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

<div class="home-proof-grid" role="list" aria-label="osmemのローカル計測ハイライト">
  <article class="home-proof-card home-proof-card--lead" role="listitem">
    <span class="home-proof-label">日本語有効の起動時間 · 5回平均</span>
    <strong>680 <small>ms</small></strong>
    <span class="home-proof-detail">seed読込後、日本語analyzerも実行</span>
  </article>
  <article class="home-proof-card" role="listitem">
    <span class="home-proof-label">server memory · RSS平均</span>
    <strong>160 <small>MiB</small></strong>
    <span class="home-proof-detail">小さな日本語seed、macOS arm64</span>
  </article>
  <article class="home-proof-card" role="listitem">
    <span class="home-proof-label">Go appへのlink増分 · osmem + 日本語</span>
    <strong>+35.0 <small>MiB</small></strong>
    <span class="home-proof-detail">最小Go実行ファイルとの差</span>
  </article>
  <article class="home-proof-card" role="listitem">
    <span class="home-proof-label">OpenSearch image · Docker Hub圧縮値</span>
    <strong>739 <small>MB</small></strong>
    <span class="home-proof-detail">download footprint。cache済みなら転送量は減る</span>
  </article>
</div>

## テストごとに新しい状態を。検索engineは起動し直さない。

osmemはseed済みbaseを一度起動し、テストごとにforkします。最初はindexを共有し、テストが変更するindexだけをcopy-on-writeで複製します。OpenSearch containerもclass全体で共有できますが、このcopy-on-write分離は標準では備えていません。

<div class="home-chart-grid">
  <section class="home-chart-card" aria-labelledby="home-startup-title">
    <h2 id="home-startup-title">テストserverが使えるまでの平均</h2>
    <p>各経路5回のローカル計測。短いほど速い。</p>
    <div class="home-bar-chart" role="list" aria-label="起動時間平均: osmem 0.68秒、Docker 6.07秒、Testcontainers 8.12秒">
      <div class="home-bar-row" role="listitem">
        <span>osmem · 日本語有効</span><strong>0.68秒</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar home-bar--osmem" style="--bar-size: 8.4%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Docker · OpenSearch</span><strong>6.07秒</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 74.8%"></span></span>
      </div>
      <div class="home-bar-row" role="listitem">
        <span>Testcontainers · OpenSearch</span><strong>8.12秒</strong>
        <span class="home-bar-track" aria-hidden="true"><span class="home-bar" style="--bar-size: 100%"></span></span>
      </div>
    </div>
  </section>

  <section class="home-chart-card" aria-labelledby="home-memory-title">
    <h2 id="home-memory-title">server起動中のmemory</h2>
    <p>設定heapではなくresident memory。Testcontainersの合計にはtest JVMを含みます。</p>
    <div class="home-bar-chart" role="list" aria-label="RSS平均: osmem 160MiB、Docker OpenSearch 942MiB、Testcontainersとtest JVM 1031MiB">
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
  <strong>Devboxはtoolbox。検索serverではありません。</strong>
  <span>warm状態の<code>devbox run</code>は平均156ms。Maven/JDK環境の初回downloadは194.1MiB（展開後351.7MiB）で、Devbox自体はOpenSearchを起動しません。</span>
</aside>

<p class="home-method-note">いずれも単一マシンの平均値で、厳密なengine比較ではありません。Docker imageは取得済み。Testcontainersは試行ごとにcontainerを新規起動しましたが、class内で共有するsuiteでは起動を按分できます。osmemのRSSには日本語analyzerを含みます。<a href="/osmem/ja/performance/">計測条件と詳細を見る</a>。</p>
