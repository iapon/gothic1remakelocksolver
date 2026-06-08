# &#128274; Gothic 1 Remake — Lock Pick Solver

> Just made an online calculator for the lock picking minigame in Gothic 1 Remake.
> Configure your puzzle, hit solve, follow the steps. Done.

**&#127760; Try it: [iapon.github.io/gothic1remakelocksolver](https://iapon.github.io/gothic1remakelocksolver/)**

## What it does

Stuck on a lock? This tool finds the shortest sequence of plate moves to align all pins at the center.

1. **Set up** your puzzle — number of plates, pin positions, which plate affects which
2. **Solve** — BFS finds the optimal path
3. **Follow** — step-by-step with visual preview of every move

## Features

- &#9881; Configurable plates, positions, and directional effects
- &#128270; BFS solver — always finds the shortest solution
- &#128065; Full path rendered instantly with mini-visualizations
- &#128190; Save &amp; load recipes (browser storage + JSON files)
- &#128269; Searchable recipe dropdown
- &#128260; Export/import all recipes at once
- &#127912; Click plates directly to set pin positions

## How to use

1. Set the number of **plates** and **positions**
2. For each plate, set the **pin position** and **effects** (which other plates it moves, same or opposite direction)
3. Hit **Find Solution**
4. Follow the moves — click &#9654; to step through, or use **Auto Play**

## License

MIT
