import { createFileRoute } from "@tanstack/react-router";

export const Route = createFileRoute("/")({
  component: IndexRoute,
});

function IndexRoute() {
  return (
    <main className="lifecycle-screen">
      <section className="lifecycle-card">
        <h1>Portal Shell</h1>
        <p>Selecting the live portal view…</p>
      </section>
    </main>
  );
}
