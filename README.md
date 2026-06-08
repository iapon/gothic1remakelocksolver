# Gothic 1 Remake Lock Pick Solver

Interactive tool for solving lock pick puzzles from Gothic 1 Remake.

## Features

- Configure number of plates, positions, and pin effects
- BFS solver finds optimal solution
- Step-by-step visualization with full path preview
- Save/load recipes (localStorage + JSON export/import)
- Searchable recipe dropdown

## Usage

Open `index.html` in any browser, or visit the GitHub Pages link.

### How it works

1. Set up your puzzle: number of plates, positions, center pin, initial pin positions
2. Define which plate moves which others (same or opposite direction)
3. Click **Find Solution** to get the shortest sequence of moves
4. Follow the steps to pick the lock

## License

MIT
