const path = require("path");

module.exports = {
	mode: "production",		// Change to "development" for debugging purposes
	entry: "./webui/js/app.js",
	output: {
		filename: "dbhub.js",
		path: path.resolve(__dirname, "js")
	},
	module: {
		rules: [
			{
				test: /\.css$/,
				use: [
					"style-loader",
					"css-loader"
				]
			}, {
				test: /\.(scss)$/,
				use: [
					{
						loader: "style-loader"
					}, {
						loader: "css-loader"
					}, {
						loader: "postcss-loader",
						options: {
							postcssOptions: {
								plugins: () => [
									require("autoprefixer")
								]
							}
						}
					}, {
						loader: "sass-loader"
					}
				]
			}
		]
	}
};
